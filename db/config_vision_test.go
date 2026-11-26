package db

import "testing"

// 视觉三态与探测缓存要能整条链路往返：profileCols（列表）/ profileColsKey（单行）
// 两套列清单都不能错位；普通保存写 vision_mode、但不许碰探测列（否则一次编辑
// 就把有效缓存清空，下一次保存又要白烧一次图片请求）。
func TestProfileVisionRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })

	id, err := d.SaveProfile(&LLMProfile{
		Name: "t-vision", Format: "openai", Model: "m", APIKey: "k",
		VisionMode: "on",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })

	p, err := d.ProfileByID(id)
	if err != nil || p == nil {
		t.Fatalf("ProfileByID: %v, p=%v", err, p)
	}
	if p.VisionMode != "on" || p.VisionProbe != "" || p.VisionProbeSig != "" {
		t.Fatalf("keyed load: mode=%q probe=%q sig=%q", p.VisionMode, p.VisionProbe, p.VisionProbeSig)
	}

	ps, err := d.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	var found *LLMProfile
	for _, x := range ps {
		if x.ID == id {
			found = x
		}
	}
	if found == nil {
		t.Fatal("profile missing from ListProfiles")
	}
	if found.VisionMode != "on" {
		t.Fatalf("list load: vision_mode=%q", found.VisionMode)
	}

	// Probe cache survives an unrelated save (blank key branch included).
	if err := d.SetVisionProbe(id, "off", "sig-1"); err != nil {
		t.Fatal(err)
	}
	p.APIKey = ""
	p.VisionMode = "" // back to auto
	if _, err := d.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	after, err := d.ProfileByID(id)
	if err != nil || after == nil {
		t.Fatalf("reload: %v", err)
	}
	if after.VisionMode != "" {
		t.Fatalf("save must persist the tri-state, got %q", after.VisionMode)
	}
	if after.VisionProbe != "off" || after.VisionProbeSig != "sig-1" {
		t.Fatalf("save must not clear the probe cache, got %q/%q", after.VisionProbe, after.VisionProbeSig)
	}
	if after.APIKey != "k" {
		t.Fatalf("blank key on update must keep the stored one, got %q", after.APIKey)
	}
}

// 非法的三态/探测值必须在写库前就拒绝，别把 DB CHECK 的 500 甩给调用方。
// 合法值由数据库 CHECK 兜底（这里只验证应用层的探测写入约束）。
func TestSetVisionProbeRejectsUnknownValue(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })

	id, err := d.SaveProfile(&LLMProfile{Name: "t-vision-reject", Format: "openai", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM llm_profiles WHERE id=$1`, id) })

	if err := d.SetVisionProbe(id, "maybe", "sig"); err == nil {
		t.Fatal("非法探测值必须被拒绝")
	}
	if err := d.SetVisionProbe(id, "", ""); err != nil {
		t.Fatalf("清空探测结果应成功: %v", err)
	}
}
