package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chu0119/pojun/db"
	"github.com/chu0119/pojun/eval"
)

// runBench 是 P1-4 评测 harness 的离线入口。
//
//	pojun-maint bench score --config ... --scenario dvwa-sqli-xss --task 18 [--out run.json]
//	pojun-maint bench validate [--scenario <dir>|builtin-id]
//	pojun-maint bench compare --a run-a.json --b run-b.json [--out diff.json] [--markdown diff.md]
//	pojun-maint bench retrieval --config ... [--golden golden.jsonl] [--out metrics.json] [--build-golden]
//
// score 只读任务数据：不重跑 agent、不改 DB、不发网络请求。
// retrieval 只读知识库：默认跑 eval.BuiltinGoldenQueries()（3 个随包 kb-pack 的
// 小样本），不访问外网；--build-golden 用库内 approved 标题生成自检基线。
func runBench(args []string) {
	if len(args) == 0 {
		benchUsage()
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("bench "+sub, flag.ExitOnError)
	cfgPath := fs.String("config", "/opt/pojun/config.json", "POJUN config.json 路径")
	scenarioPath := fs.String("scenario", "", "场景 manifest 文件或目录")
	taskID := fs.Int64("task", 0, "score: 要评分的任务 ID")
	outPath := fs.String("out", "", "输出 JSON 文件")
	aPath := fs.String("a", "", "compare: 基线 RunResult JSON")
	bPath := fs.String("b", "", "compare: 新 RunResult JSON")
	mdPath := fs.String("markdown", "", "compare: 额外输出 Markdown 报告")
	goldenPath := fs.String("golden", "", "retrieval: golden JSONL（缺省用内置小样本）")
	buildGolden := fs.Bool("build-golden", false, "retrieval: 用库内 approved 标题生成 golden JSONL 到 --out")
	goldenLimit := fs.Int("limit", 120, "retrieval --build-golden: 最多生成条数")
	_ = fs.Parse(args[1:])

	switch sub {
	case "validate":
		scs, err := resolveScenarios(*scenarioPath)
		if err != nil {
			fail(err)
		}
		for _, sc := range scs {
			fmt.Printf("OK  %-40s required=%d optional=%d stages=%d\n",
				sc.ID, sc.RequiredCount(), len(sc.Expected)-sc.RequiredCount(), len(sc.Stages))
		}
		fmt.Printf("共 %d 个场景，全部通过校验\n", len(scs))

	case "score":
		if strings.TrimSpace(*scenarioPath) == "" || *taskID <= 0 {
			fail(fmt.Errorf("score 需要 --scenario 与 --task"))
		}
		sc, err := resolveScenario(*scenarioPath)
		if err != nil {
			fail(err)
		}
		dsn, err := loadDSN(*cfgPath)
		if err != nil {
			fail(err)
		}
		pg, err := db.OpenReadOnly(dsn)
		if err != nil {
			fail(fmt.Errorf("连接数据库失败：%w", err))
		}
		defer pg.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := eval.ScoreTask(ctx, pg, sc, *taskID)
		if err != nil {
			fail(err)
		}
		printRunResult(res)
		if *outPath != "" {
			if err := eval.WriteJSON(*outPath, res); err != nil {
				fail(err)
			}
			fmt.Printf("已写入 %s\n", *outPath)
		}

	case "compare":
		if strings.TrimSpace(*aPath) == "" || strings.TrimSpace(*bPath) == "" {
			fail(fmt.Errorf("compare 需要 --a 与 --b"))
		}
		a, err := eval.ReadRunResult(*aPath)
		if err != nil {
			fail(err)
		}
		b, err := eval.ReadRunResult(*bPath)
		if err != nil {
			fail(err)
		}
		cmp, err := eval.CompareRuns(a, b)
		if err != nil {
			fail(err)
		}
		md := cmp.Markdown()
		fmt.Print(md)
		if *outPath != "" {
			if err := eval.WriteJSON(*outPath, cmp); err != nil {
				fail(err)
			}
			fmt.Printf("已写入 %s\n", *outPath)
		}
		if *mdPath != "" {
			if err := os.WriteFile(*mdPath, []byte(md), 0o644); err != nil {
				fail(err)
			}
			fmt.Printf("已写入 %s\n", *mdPath)
		}
		if cmp.Regressed {
			os.Exit(1) // 回归时给 CI/脚本一个非零退出码
		}

	case "retrieval":
		dsn, err := loadDSN(*cfgPath)
		if err != nil {
			fail(err)
		}
		pg, err := db.OpenReadOnly(dsn)
		if err != nil {
			fail(fmt.Errorf("连接数据库失败：%w", err))
		}
		defer pg.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		if *buildGolden {
			queries, err := eval.BuildGoldenFromKB(ctx, pg.KB(), *goldenLimit)
			if err != nil {
				fail(err)
			}
			if *outPath == "" {
				fail(fmt.Errorf("--build-golden 需要 --out <golden.jsonl>"))
			}
			if err := eval.WriteGoldenQueries(*outPath, queries); err != nil {
				fail(err)
			}
			fmt.Printf("已用 %d 条 approved 标题生成 %s\n", len(queries), *outPath)
			return
		}
		queries := eval.BuiltinGoldenQueries()
		source := "builtin"
		if strings.TrimSpace(*goldenPath) != "" {
			queries, err = eval.LoadGoldenQueries(*goldenPath)
			if err != nil {
				fail(err)
			}
			source = *goldenPath
		}
		if len(queries) == 0 {
			fail(fmt.Errorf("没有可评测的查询（golden 为空）"))
		}
		m := eval.EvaluateQueries(ctx, pg.KB(), queries)
		fmt.Printf("检索评测：%d 条查询（来源 %s）\n", len(queries), source)
		fmt.Printf("Recall@5=%.3f  MRR@10=%.3f  nDCG@10=%.3f  无结果率=%.1f%%  P95=%.0fms\n",
			m.RecallAt5, m.MRRAt10, m.NDCGAt10, m.NoResultRate*100, m.P95LatencyMS)
		if m.RecallAt5 == 0 && m.NoResultRate > 0 {
			fmt.Println("提示：内置样本依赖随包 kb-pack 已导入且为 approved。若库是空的，先运行 pojun-maint library import。")
		}
		if *outPath != "" {
			if err := eval.WriteJSON(*outPath, m); err != nil {
				fail(err)
			}
			fmt.Printf("已写入 %s\n", *outPath)
		}

	default:
		benchUsage()
		os.Exit(2)
	}
}

// resolveScenario 支持三种写法：manifest 文件路径、内置场景 ID、以及（仅
// validate 用的）目录。内置场景通过 go:embed 随二进制分发，部署机上不需要
// 额外上传 eval/scenarios。
func resolveScenario(arg string) (*eval.Scenario, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil, fmt.Errorf("--scenario 不能为空")
	}
	if _, err := os.Stat(arg); err == nil {
		return eval.LoadScenario(arg)
	}
	return eval.BuiltinScenario(arg)
}

func resolveScenarios(arg string) ([]*eval.Scenario, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return eval.BuiltinScenarios()
	}
	if st, err := os.Stat(arg); err == nil {
		if st.IsDir() {
			return eval.LoadScenarios(arg)
		}
		sc, err := eval.LoadScenario(arg)
		if err != nil {
			return nil, err
		}
		return []*eval.Scenario{sc}, nil
	}
	sc, err := eval.BuiltinScenario(arg)
	if err != nil {
		return nil, err
	}
	return []*eval.Scenario{sc}, nil
}

func printRunResult(r *eval.RunResult) {
	fmt.Printf("场景: %s (%s)\n", r.ScenarioID, r.ScenarioName)
	fmt.Printf("任务: #%d  状态=%s  目标=%d/%d\n", r.TaskID, r.Status, r.GoalsMet, r.GoalsTotal)
	fmt.Printf("结果: %s  %s\n", passText(r.Pass), r.PassReason)
	fmt.Printf("必得项: %d/%d  预期命中(含可选): %d/%d  额外发现: %d  额外率: %.1f%%\n",
		r.RequiredFound, r.RequiredTotal, r.ExpectedFound, len(r.Expected), r.UnexpectedN, r.ExtraRate*100)
	fmt.Printf("证据完整度: %.1f%%  阶段覆盖: %d/%d\n", r.EvidenceCompleteness*100, r.StagesSeen, r.StageTotal)
	dur := fmt.Sprintf("%.1f 分钟", r.DurationMinutes)
	if !r.DurationComparable {
		dur += "（未终态，不参与超时判定）"
	}
	fmt.Printf("Token: in=%d out=%d  成本: $%.4f (known=%v)  耗时: %s  附件: %d\n",
		r.TokensInput, r.TokensOutput, r.CostUSD, r.CostKnown, dur, r.Artifacts)
	if len(r.BudgetNote) > 0 {
		fmt.Printf("预算提示: %s\n", strings.Join(r.BudgetNote, "；"))
	}
	for _, e := range r.Expected {
		mark := "✗"
		if e.Matched {
			mark = "✓"
		}
		opt := ""
		if e.Optional {
			opt = " (optional)"
		}
		fmt.Printf("  %s %s%s  证据=%d/%d\n", mark, e.VulnClass, opt, e.EvidenceRunes, e.MinEvidenceRunes)
	}
}

func passText(v bool) string {
	if v {
		return "PASS"
	}
	return "FAIL"
}

func benchUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  pojun-maint bench score    --config <config.json> --scenario <builtin-id|scenario.json> --task <id> [--out run.json]
  pojun-maint bench validate [--scenario <scenarios-dir|builtin-id>]
  pojun-maint bench compare  --a <baseline.json> --b <new.json> [--out diff.json] [--markdown diff.md]
  pojun-maint bench retrieval --config <config.json> [--golden <golden.jsonl>] [--out metrics.json]
  pojun-maint bench retrieval --config <config.json> --build-golden --out <golden.jsonl> [--limit 120]`)
}
