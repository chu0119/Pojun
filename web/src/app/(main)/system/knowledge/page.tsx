"use client";

// 知识库管理页（KB v2）。
//
// 数据流：知识包/agent 回写默认落草稿，人工在这里采纳后才会进入默认检索。
// 页面同时展示检索质量摘要（近 N 天零结果率/P95/归因率）与 facets（产品/
// 漏洞类/平台/CVE），方便从「哪些问题没人查」「哪些维度条目多」反推补条目。

import * as React from "react";
import {
  BookOpenIcon,
  CheckIcon,
  ChevronLeftIcon,
  ChevronRightIcon,
  Loader2Icon,
  PencilIcon,
  PlusIcon,
  SearchIcon,
  Trash2Icon,
} from "lucide-react";
import { toast } from "sonner";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type {
  KnowledgeEntry,
  KnowledgeEntryInput,
  KnowledgeFacets,
  KnowledgeRetrievalStats,
} from "@/lib/types";
import { cn } from "@/lib/utils";

const PAGE_SIZES = [10, 20, 50, 100];

const LAYER_LABELS: Record<string, string> = {
  general: "通用",
  object: "对象",
  infra: "基础设施",
  weakness: "弱点",
  evidence: "证据",
};

const STATUS_LABELS: Record<string, string> = {
  draft: "草稿",
  approved: "已采纳",
  deprecated: "已弃用",
};

const ORIGIN_LABELS: Record<string, string> = {
  seed: "内置/知识包",
  learned: "实战回写",
  manual: "人工录入",
};

const EMPTY_FACETS: KnowledgeFacets = {
  products: [],
  vuln_classes: [],
  platforms: [],
  cve_ids: [],
};

type FormState = {
  title: string;
  description: string;
  detail: string;
  commandsText: string;
  tagsText: string;
  layer: string;
  status: string;
  confidence: string;
  source: string;
  productsText: string;
  vulnClassesText: string;
  platformsText: string;
  cveIdsText: string;
  license: string;
};

const EMPTY_FORM: FormState = {
  title: "",
  description: "",
  detail: "",
  commandsText: "",
  tagsText: "",
  layer: "general",
  status: "approved",
  confidence: "observed",
  source: "",
  productsText: "",
  vulnClassesText: "",
  platformsText: "",
  cveIdsText: "",
  license: "",
};

function splitList(s: string): string[] {
  return s
    .split(/[,，\s]+/)
    .map((v) => v.trim().toLowerCase())
    .filter(Boolean);
}

function entryToForm(e: KnowledgeEntry): FormState {
  return {
    title: e.title,
    description: e.description,
    detail: e.detail,
    commandsText: (e.commands ?? []).join("\n"),
    tagsText: (e.tags ?? []).join(", "),
    layer: e.layer || "general",
    status: e.status || "draft",
    confidence: e.confidence || "observed",
    source: e.source ?? "",
    productsText: (e.products ?? []).join(", "),
    vulnClassesText: (e.vuln_classes ?? []).join(", "),
    platformsText: (e.platforms ?? []).join(", "),
    cveIdsText: (e.cve_ids ?? []).join(", "),
    license: e.license ?? "",
  };
}

function formToInput(f: FormState): KnowledgeEntryInput {
  return {
    title: f.title.trim(),
    description: f.description.trim(),
    detail: f.detail,
    commands: f.commandsText
      .split("\n")
      .map((s) => s.trim())
      .filter(Boolean),
    tags: splitList(f.tagsText),
    layer: f.layer,
    status: f.status,
    confidence: f.confidence,
    source: f.source.trim(),
    products: splitList(f.productsText),
    vuln_classes: splitList(f.vulnClassesText),
    platforms: splitList(f.platformsText),
    cve_ids: splitList(f.cveIdsText).map((v) => v.toUpperCase()),
    license: f.license.trim(),
  };
}

function statusBadge(status: string) {
  switch (status) {
    case "approved":
      return (
        <Badge variant="outline" className="border-emerald-500/40 text-emerald-600">
          已采纳
        </Badge>
      );
    case "deprecated":
      return (
        <Badge variant="outline" className="text-muted-foreground">
          已弃用
        </Badge>
      );
    default:
      return (
        <Badge variant="outline" className="border-amber-500/40 text-amber-600">
          草稿
        </Badge>
      );
  }
}

function fmtTime(ts?: string) {
  if (!ts) return "-";
  return new Date(ts).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function qualityBadge(score?: number) {
  if (typeof score !== "number") {
    return <span className="text-muted-foreground text-xs">—</span>;
  }
  return (
    <Badge
      variant={score >= 0.7 ? "default" : score >= 0.4 ? "secondary" : "outline"}
      className="font-normal text-[10px] tabular-nums"
      title="启发式质量分（正文/命令/来源/证据强度）"
    >
      {(score * 100).toFixed(0)}
    </Badge>
  );
}

export default function KnowledgePage() {
  const [items, setItems] = React.useState<KnowledgeEntry[]>([]);
  const [total, setTotal] = React.useState(0);
  const [page, setPage] = React.useState(1);
  const [size, setSize] = React.useState(20);
  const [query, setQuery] = React.useState("");
  const [debouncedQuery, setDebouncedQuery] = React.useState("");
  const [status, setStatus] = React.useState("all");
  const [layer, setLayer] = React.useState("all");
  const [product, setProduct] = React.useState("all");
  const [vulnClass, setVulnClass] = React.useState("all");
  const [minQuality, setMinQuality] = React.useState("0");
  const [loading, setLoading] = React.useState(false);
  const [stats, setStats] = React.useState<Record<string, number>>({});
  const [facets, setFacets] = React.useState<KnowledgeFacets>(EMPTY_FACETS);
  const [retrieval, setRetrieval] = React.useState<KnowledgeRetrievalStats | null>(null);

  const [sheetOpen, setSheetOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<KnowledgeEntry | null>(null);
  const [form, setForm] = React.useState<FormState>(EMPTY_FORM);
  const [saving, setSaving] = React.useState(false);
  const [pendingDelete, setPendingDelete] = React.useState<KnowledgeEntry | null>(null);

  React.useEffect(() => {
    const t = setTimeout(() => {
      setDebouncedQuery(query.trim());
      setPage(1);
    }, 300);
    return () => clearTimeout(t);
  }, [query]);

  const load = React.useCallback(() => {
    let alive = true;
    setLoading(true);
    api
      .knowledgeList({
        q: debouncedQuery || undefined,
        status: status === "all" ? undefined : status,
        layer: layer === "all" ? undefined : layer,
        product: product === "all" ? undefined : product,
        vuln_class: vulnClass === "all" ? undefined : vulnClass,
        min_quality: Number(minQuality) > 0 ? Number(minQuality) : undefined,
        page,
        page_size: size,
      })
      .then((r) => {
        if (!alive) return;
        setItems(r.entries ?? []);
        setTotal(r.total);
      })
      .catch(() => {
        /* 列表加载失败：保留上一次结果，不打断页面 */
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [debouncedQuery, status, layer, product, vulnClass, minQuality, page, size]);

  React.useEffect(() => load(), [load]);

  const loadMeta = React.useCallback(() => {
    api
      .knowledgeStats()
      .then(setStats)
      .catch(() => {
        /* 统计失败不影响主列表 */
      });
    api
      .knowledgeFacets()
      .then(setFacets)
      .catch(() => {
        /* facets 失败时筛选器仍可用「全部」 */
      });
    api
      .knowledgeRetrievalStats(7)
      .then(setRetrieval)
      .catch(() => {
        /* 检索日志为空/接口失败时不显示质量卡 */
      });
  }, []);

  React.useEffect(() => loadMeta(), [loadMeta]);

  const totalPages = Math.max(1, Math.ceil(total / size));

  const openCreate = () => {
    setEditing(null);
    setForm(EMPTY_FORM);
    setSheetOpen(true);
  };

  const openEdit = (e: KnowledgeEntry) => {
    setEditing(e);
    setForm(entryToForm(e));
    setSheetOpen(true);
  };

  const save = () => {
    if (!form.title.trim()) {
      toast.error("标题不能为空");
      return;
    }
    setSaving(true);
    const body = formToInput(form);
    const req = editing ? api.updateKnowledge(editing.id, body) : api.createKnowledge(body);
    req
      .then(() => {
        toast.success(editing ? "已保存" : "已创建");
        setSheetOpen(false);
        load();
        loadMeta();
      })
      .catch((e: Error) => toast.error(`保存失败：${e.message}`))
      .finally(() => setSaving(false));
  };

  const quickStatus = (e: KnowledgeEntry, next: string) => {
    api
      .updateKnowledge(e.id, { status: next })
      .then(() => {
        toast.success(next === "approved" ? "已采纳，之后会被 agent 检索到" : "已更新状态");
        load();
        loadMeta();
      })
      .catch((err: Error) => toast.error(`操作失败：${err.message}`));
  };

  const remove = () => {
    if (!pendingDelete) return;
    api
      .deleteKnowledge(pendingDelete.id)
      .then(() => {
        toast.success("已删除");
        setPendingDelete(null);
        load();
        loadMeta();
      })
      .catch((err: Error) => toast.error(`删除失败：${err.message}`));
  };

  const statChip = (label: string, key: string, cls: string) => (
    <div key={key} className="flex items-center gap-2 rounded-md border px-3 py-1.5">
      <span className="text-muted-foreground text-xs">{label}</span>
      <span className={cn("font-semibold text-sm tabular-nums", cls)}>{stats[key] ?? 0}</span>
    </div>
  );

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <BookOpenIcon className="h-5 w-5 text-muted-foreground" />
          <h1 className="font-semibold text-xl tracking-tight">知识库</h1>
          <Badge variant="secondary">{total}</Badge>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {statChip("已采纳", "approved", "text-emerald-600")}
          {statChip("草稿", "draft", "text-amber-600")}
          {statChip("已弃用", "deprecated", "text-muted-foreground")}
          <Button size="sm" onClick={openCreate}>
            <PlusIcon className="size-4" />
            新建
          </Button>
        </div>
      </div>

      {retrieval ? (
        <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
          <div className="rounded-md border px-3 py-2">
            <div className="text-muted-foreground text-xs">近 {retrieval.days} 天检索</div>
            <div className="font-semibold text-lg tabular-nums">{retrieval.total}</div>
          </div>
          <div className="rounded-md border px-3 py-2">
            <div className="text-muted-foreground text-xs">无结果率</div>
            <div className="font-semibold text-lg tabular-nums">
              {(retrieval.zero_result_rate * 100).toFixed(1)}%
            </div>
          </div>
          <div className="rounded-md border px-3 py-2">
            <div className="text-muted-foreground text-xs">P95 延迟</div>
            <div className="font-semibold text-lg tabular-nums">
              {Math.round(retrieval.p95_latency_ms)}ms
            </div>
          </div>
          <div className="rounded-md border px-3 py-2">
            <div className="text-muted-foreground text-xs">Actor+Task 归因率</div>
            <div className="font-semibold text-lg tabular-nums">
              {(retrieval.attributed_rate * 100).toFixed(1)}%
            </div>
          </div>
        </div>
      ) : null}

      <div className="flex flex-wrap items-center gap-2">
        <div className="relative max-w-sm flex-1">
          <SearchIcon className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            placeholder="搜索标题/正文（中文按二元组匹配）…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="h-8 pl-8"
          />
        </div>
        <Select
          value={status}
          onValueChange={(v) => {
            setStatus(v);
            setPage(1);
          }}
        >
          <SelectTrigger size="sm" className="w-32">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部状态</SelectItem>
            <SelectItem value="approved">已采纳</SelectItem>
            <SelectItem value="draft">草稿</SelectItem>
            <SelectItem value="deprecated">已弃用</SelectItem>
          </SelectContent>
        </Select>
        <Select
          value={layer}
          onValueChange={(v) => {
            setLayer(v);
            setPage(1);
          }}
        >
          <SelectTrigger size="sm" className="w-32">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部分层</SelectItem>
            {Object.entries(LAYER_LABELS).map(([k, v]) => (
              <SelectItem key={k} value={k}>
                {v}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          value={product}
          onValueChange={(v) => {
            setProduct(v);
            setPage(1);
          }}
        >
          <SelectTrigger size="sm" className="w-36">
            <SelectValue placeholder="产品" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部产品</SelectItem>
            {facets.products.map((f) => (
              <SelectItem key={f.value} value={f.value}>
                {f.value} ({f.count})
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          value={vulnClass}
          onValueChange={(v) => {
            setVulnClass(v);
            setPage(1);
          }}
        >
          <SelectTrigger size="sm" className="w-36">
            <SelectValue placeholder="漏洞类" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部漏洞类</SelectItem>
            {facets.vuln_classes.map((f) => (
              <SelectItem key={f.value} value={f.value}>
                {f.value} ({f.count})
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          value={minQuality}
          onValueChange={(v) => {
            setMinQuality(v);
            setPage(1);
          }}
        >
          <SelectTrigger size="sm" className="w-32">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="0">质量不限</SelectItem>
            <SelectItem value="0.4">质量 ≥40</SelectItem>
            <SelectItem value="0.7">质量 ≥70</SelectItem>
          </SelectContent>
        </Select>
        <Select value={String(size)} onValueChange={(v) => setSize(Number(v))}>
          <SelectTrigger size="sm" className="w-28">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PAGE_SIZES.map((n) => (
              <SelectItem key={n} value={String(n)}>
                {n} / 页
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <div className="ml-auto flex items-center gap-2 text-muted-foreground text-xs">
          {loading && <Loader2Icon className="size-3.5 animate-spin" />}
          <span className="tabular-nums">
            {total === 0 ? 0 : (page - 1) * size + 1}–{(page - 1) * size + items.length} / {total}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="size-8"
            disabled={page <= 1}
            onClick={() => setPage((p) => Math.max(1, p - 1))}
          >
            <ChevronLeftIcon />
          </Button>
          <span className="tabular-nums">
            {page} / {totalPages}
          </span>
          <Button
            variant="outline"
            size="icon"
            className="size-8"
            disabled={page >= totalPages}
            onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
          >
            <ChevronRightIcon />
          </Button>
        </div>
      </div>

      <Card className="min-h-0 flex-1 overflow-y-auto overflow-x-hidden py-0">
        <Table className="table-fixed">
          <TableHeader>
            <TableRow>
              <TableHead>标题</TableHead>
              <TableHead className="hidden w-16 sm:table-cell">分层</TableHead>
              <TableHead className="hidden w-20 sm:table-cell">状态</TableHead>
              <TableHead className="hidden w-28 md:table-cell">来源</TableHead>
              <TableHead className="hidden w-40 lg:table-cell">产品 / 漏洞类</TableHead>
              <TableHead className="hidden w-14 text-right md:table-cell">质量</TableHead>
              <TableHead className="hidden w-12 text-right sm:table-cell">采纳</TableHead>
              <TableHead className="hidden w-28 lg:table-cell">更新</TableHead>
              <TableHead className="w-24 text-right">操作</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {items.length === 0 ? (
              <TableRow>
                <TableCell colSpan={9} className="h-32 whitespace-normal text-center text-muted-foreground text-sm">
                  {loading ? "加载中…" : "暂无知识条目。启动时会导入随包分发的 3 个 kb-pack（草稿/已采纳按 manifest）；也可点「新建」手工录入。"}
                </TableCell>
              </TableRow>
            ) : (
              items.map((e) => (
                <TableRow key={e.id} className="align-top">
                  <TableCell className="min-w-0">
                    <div className="truncate font-medium text-sm" title={e.title}>
                      {e.title}
                    </div>
                    {e.description ? (
                      <div className="mt-0.5 line-clamp-1 text-muted-foreground text-xs">
                        {e.description}
                      </div>
                    ) : null}
                    <div className="mt-1 flex flex-wrap items-center gap-1 text-[10px] text-muted-foreground sm:hidden">
                      <span>{STATUS_LABELS[e.status] ?? e.status}</span>
                      <span>·</span>
                      <span>{LAYER_LABELS[e.layer] ?? e.layer}</span>
                      <span>·</span>
                      <span>{ORIGIN_LABELS[e.origin] ?? e.origin}</span>
                      <span>·</span>
                      <span>{fmtTime(e.updated_at)}</span>
                    </div>
                  </TableCell>
                  <TableCell className="hidden sm:table-cell">
                    <span className="text-xs">{LAYER_LABELS[e.layer] ?? e.layer}</span>
                  </TableCell>
                  <TableCell className="hidden sm:table-cell">{statusBadge(e.status)}</TableCell>
                  <TableCell className="hidden md:table-cell">
                    <div className="text-xs">{ORIGIN_LABELS[e.origin] ?? e.origin}</div>
                    {e.source ? (
                      <div className="max-w-28 truncate text-[10px] text-muted-foreground" title={e.source}>
                        {e.source}
                      </div>
                    ) : null}
                  </TableCell>
                  <TableCell className="hidden lg:table-cell">
                    <div className="flex flex-wrap gap-1">
                      {(e.products ?? []).slice(0, 2).map((p) => (
                        <Badge key={`p-${p}`} variant="secondary" className="font-normal text-[10px]">
                          {p}
                        </Badge>
                      ))}
                      {(e.vuln_classes ?? []).slice(0, 2).map((c) => (
                        <Badge key={`c-${c}`} variant="outline" className="font-normal text-[10px]">
                          {c}
                        </Badge>
                      ))}
                    </div>
                  </TableCell>
                  <TableCell className="hidden text-right md:table-cell">{qualityBadge(e.quality_score)}</TableCell>
                  <TableCell className="hidden text-right tabular-nums sm:table-cell">{e.uses}</TableCell>
                  <TableCell className="hidden text-muted-foreground text-xs lg:table-cell">
                    {fmtTime(e.updated_at)}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      {e.status !== "approved" && (
                        <Button
                          variant="ghost"
                          size="icon"
                          className="size-7"
                          title="采纳（之后会被 agent 检索）"
                          onClick={() => quickStatus(e, "approved")}
                        >
                          <CheckIcon className="size-3.5" />
                        </Button>
                      )}
                      <Button
                        variant="ghost"
                        size="icon"
                        className="size-7"
                        title="编辑"
                        onClick={() => openEdit(e)}
                      >
                        <PencilIcon className="size-3.5" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        className="size-7 text-destructive"
                        title="删除"
                        onClick={() => setPendingDelete(e)}
                      >
                        <Trash2Icon className="size-3.5" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      <Sheet open={sheetOpen} onOpenChange={setSheetOpen}>
        <SheetContent className="flex w-full flex-col overflow-y-auto data-[side=right]:w-full! data-[side=right]:sm:max-w-2xl!">
          <SheetHeader>
            <SheetTitle>{editing ? "编辑知识条目" : "新建知识条目"}</SheetTitle>
            <SheetDescription>
              {editing
                ? `#${editing.id} · ${ORIGIN_LABELS[editing.origin] ?? editing.origin} · 采纳后 agent 才能检索到`
                : "人工录入默认直接采纳。标题是去重键：同标题不会重复创建。"}
            </SheetDescription>
          </SheetHeader>

          <div className="flex flex-col gap-4 px-1 py-2">
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="kb-title">标题（必填，同标题视为同一条）</Label>
              <Input
                id="kb-title"
                value={form.title}
                onChange={(e) => setForm({ ...form, title: e.target.value })}
                placeholder="如：Nacos 未授权/默认 token 密钥"
              />
            </div>

            <div className="flex flex-col gap-1.5">
              <Label htmlFor="kb-desc">一句话说明</Label>
              <Input
                id="kb-desc"
                value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })}
                placeholder="这条知识解决什么问题"
              />
            </div>

            <div className="flex flex-col gap-1.5">
              <Label htmlFor="kb-detail">正文（markdown：适用条件、步骤、验证、边界）</Label>
              <Textarea
                id="kb-detail"
                value={form.detail}
                onChange={(e) => setForm({ ...form, detail: e.target.value })}
                className="min-h-40 font-mono text-xs"
              />
            </div>

            <div className="flex flex-col gap-1.5">
              <Label htmlFor="kb-cmds">可复现命令（一行一条，用 {"{{TARGET}}"} 表示目标）</Label>
              <Textarea
                id="kb-cmds"
                value={form.commandsText}
                onChange={(e) => setForm({ ...form, commandsText: e.target.value })}
                className="min-h-24 font-mono text-xs"
                placeholder={"curl -sS 'https://{{TARGET}}/nacos/'\nnuclei -u https://{{TARGET}} -tags ..."}
              />
            </div>

            <div className="grid grid-cols-2 gap-4">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="kb-tags">标签（逗号分隔）</Label>
                <Input
                  id="kb-tags"
                  value={form.tagsText}
                  onChange={(e) => setForm({ ...form, tagsText: e.target.value })}
                  placeholder="web, rce, nacos"
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label>分层</Label>
                <Select value={form.layer} onValueChange={(v) => setForm({ ...form, layer: v })}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {Object.entries(LAYER_LABELS).map(([k, v]) => (
                      <SelectItem key={k} value={k}>
                        {v}（{k}）
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>

            <div className="grid grid-cols-2 gap-4">
              <div className="flex flex-col gap-1.5">
                <Label>状态</Label>
                <Select value={form.status} onValueChange={(v) => setForm({ ...form, status: v })}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {Object.entries(STATUS_LABELS).map(([k, v]) => (
                      <SelectItem key={k} value={k}>
                        {v}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="flex flex-col gap-1.5">
                <Label>证据强度</Label>
                <Select
                  value={form.confidence}
                  onValueChange={(v) => setForm({ ...form, confidence: v })}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="observed">observed（直接看到/跑通）</SelectItem>
                    <SelectItem value="inferred">inferred（据现象推断）</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            </div>

            <div className="flex flex-col gap-1.5">
              <Label htmlFor="kb-source">来源</Label>
              <Input
                id="kb-source"
                value={form.source}
                onChange={(e) => setForm({ ...form, source: e.target.value })}
                placeholder="writeup URL / CVE 编号 / 实战:任务#id"
              />
            </div>

            <div className="grid grid-cols-2 gap-4">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="kb-products">产品 facets（逗号分隔）</Label>
                <Input
                  id="kb-products"
                  value={form.productsText}
                  onChange={(e) => setForm({ ...form, productsText: e.target.value })}
                  placeholder="nacos, spring-boot"
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="kb-classes">漏洞类 facets（逗号分隔）</Label>
                <Input
                  id="kb-classes"
                  value={form.vulnClassesText}
                  onChange={(e) => setForm({ ...form, vulnClassesText: e.target.value })}
                  placeholder="unauthorized-access, rce"
                />
              </div>
            </div>

            <div className="grid grid-cols-3 gap-4">
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="kb-platforms">平台</Label>
                <Input
                  id="kb-platforms"
                  value={form.platformsText}
                  onChange={(e) => setForm({ ...form, platformsText: e.target.value })}
                  placeholder="java, web"
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="kb-cves">CVE</Label>
                <Input
                  id="kb-cves"
                  value={form.cveIdsText}
                  onChange={(e) => setForm({ ...form, cveIdsText: e.target.value })}
                  placeholder="cve-2022-22947"
                />
              </div>
              <div className="flex flex-col gap-1.5">
                <Label htmlFor="kb-license">许可证</Label>
                <Input
                  id="kb-license"
                  value={form.license}
                  onChange={(e) => setForm({ ...form, license: e.target.value })}
                  placeholder="CC0-1.0"
                />
              </div>
            </div>
          </div>

          <SheetFooter className="flex-row justify-end gap-2">
            <Button variant="outline" onClick={() => setSheetOpen(false)}>
              取消
            </Button>
            <Button onClick={save} disabled={saving}>
              {saving && <Loader2Icon className="size-4 animate-spin" />}
              保存
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>

      <AlertDialog open={!!pendingDelete} onOpenChange={(o) => !o && setPendingDelete(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除知识条目</AlertDialogTitle>
            <AlertDialogDescription>
              将删除「{pendingDelete?.title}」。删除后不可恢复；如果只是不想让它被检索，建议改为「已弃用」。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={remove}>删除</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
