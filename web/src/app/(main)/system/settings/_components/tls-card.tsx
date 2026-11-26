"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  KeyRoundIcon,
  Loader2Icon,
  LockIcon,
  RefreshCwIcon,
  ShieldCheckIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { TLSACMEConfig, TLSMode, TLSStatusResp } from "@/lib/types";

const MODE_LABEL: Record<TLSMode, string> = {
  off: "关闭（仅 HTTP）",
  selfsigned: "自签证书（内网/测试）",
  import: "导入证书（已有域名证书）",
  acme: "ACME 自动申请（Let's Encrypt，支持 IP 证书）",
};

const SOURCE_LABEL: Record<string, string> = {
  selfsigned: "自签",
  import: "导入",
  acme: "ACME",
  file: "文件",
};

function fmtDate(s?: string): string {
  if (!s || s.startsWith("0001-")) return "—";
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleString();
}

function daysLeft(s?: string): number | null {
  if (!s || s.startsWith("0001-")) return null;
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return null;
  return Math.ceil((d.getTime() - Date.now()) / 86_400_000);
}

function splitList(s: string): string[] {
  return Array.from(
    new Set(
      s
        .split(/[\s,;]+/)
        .map((v) => v.trim())
        .filter(Boolean),
    ),
  );
}

export function TLSCard() {
  const [info, setInfo] = React.useState<TLSStatusResp | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [busy, setBusy] = React.useState<string | null>(null);
  const [loadError, setLoadError] = React.useState<string | null>(null);

  const [mode, setMode] = React.useState<TLSMode>("off");
  const [addr, setAddr] = React.useState(":8787");
  // 监听地址是否被用户改过：没改过就不提交（后端保留原值），否则状态页显示的
  // 运行地址会被当成“新地址”提交，试绑到正在监听的端口必然失败。
  const [addrTouched, setAddrTouched] = React.useState(false);
  const [hsts, setHsts] = React.useState(false);
  const [firewallAuto, setFirewallAuto] = React.useState(true);
  const [sansText, setSansText] = React.useState("");
  const [certPEM, setCertPEM] = React.useState("");
  const [keyPEM, setKeyPEM] = React.useState("");
  const [identifiers, setIdentifiers] = React.useState("");
  const [email, setEmail] = React.useState("");
  const [staging, setStaging] = React.useState(false);
  const [challenge, setChallenge] = React.useState("http-01");
  // 挑战监听地址：必须回填已有值，否则每次“保存设置”都会把自定义端口改回 80/443。
  const [httpAddr, setHttpAddr] = React.useState(":80");
  const [tlsAddr, setTlsAddr] = React.useState(":443");
  // 独立于 running 的轮询开关：POST 成功即开始轮询，避免“running 还没置位就
  // 读到 false → 不轮询 → 再点报 409”的竞态。
  const [polling, setPolling] = React.useState(false);
  const pollFails = React.useRef(0);
  const pollStart = React.useRef(0);

  const applyInfo = React.useCallback((r: TLSStatusResp) => {
    setInfo(r);
    setLoadError(null);
    setMode(r.settings.mode);
    // 输入框显示「已保存的监听地址」：直接显示运行地址会在保存新端口后被覆盖回
    // 旧值，needsRestart 随之失效（用户看不到「待重启生效」）。未保存过地址时以
    // 实际运行地址（来自 -addr）为准。
    if (r.addr_saved && r.settings.https_addr) {
      setAddr(r.settings.https_addr);
    } else {
      setAddr(r.listen_addr || r.settings.https_addr || ":8787");
    }
    setAddrTouched(false);
    setHsts(r.settings.hsts);
    setFirewallAuto(r.settings.firewall_auto);
    setSansText((r.settings.self_signed_sans || []).join("\n"));
    setIdentifiers((r.settings.acme?.identifiers || []).join("\n"));
    setEmail(r.settings.acme?.email || "");
    setStaging(!!r.settings.acme?.staging);
    setChallenge(r.settings.acme?.challenge || "http-01");
    setHttpAddr(r.settings.acme?.http_addr || ":80");
    setTlsAddr(r.settings.acme?.tls_addr || ":443");
  }, []);

  const load = React.useCallback(
    async (silent = false) => {
      if (!silent) setLoading(true);
      try {
        applyInfo(await api.tlsStatus());
      } catch (e) {
        const msg = e instanceof Error ? e.message : "读取证书状态失败";
        // 轮询也记录：连续失败要停止并让用户看到原因，不能一直停在“申请中…”。
        setLoadError(msg);
        if (!silent) toast.error(msg);
      } finally {
        if (!silent) setLoading(false);
      }
    },
    [applyInfo],
  );

  React.useEffect(() => {
    void load();
  }, [load]);

  // 申请期间轮询：由 polling 驱动（不是 running 快照）；连续失败 5 次停止。
  React.useEffect(() => {
    if (!polling) return;
    let stopped = false;
    const tick = async () => {
      // 兜底上限：状态长期卡在 running（后端异常/进程被挂起）时不要无限轮询。
      if (pollStart.current && Date.now() - pollStart.current > 15 * 60 * 1000) {
        setPolling(false);
        setLoadError("申请状态超过 15 分钟未结束，已停止轮询；请查看服务日志或稍后刷新");
        return;
      }
      try {
        const r = await api.tlsStatus();
        if (stopped) return;
        pollFails.current = 0;
        applyInfo(r);
        if (!r.status.acme.running) setPolling(false);
      } catch (e) {
        if (stopped) return;
        pollFails.current += 1;
        if (pollFails.current >= 5) {
          setPolling(false);
          setLoadError(e instanceof Error ? e.message : "状态刷新失败");
          toast.error("证书状态刷新连续失败，已停止轮询");
        }
      }
    };
    const t = setInterval(() => void tick(), 3000);
    void tick();
    return () => {
      stopped = true;
      clearInterval(t);
    };
  }, [polling, applyInfo]);

  const acmeConfig = (): TLSACMEConfig => ({
    identifiers: splitList(identifiers),
    email: email.trim(),
    staging,
    challenge,
    http_addr: httpAddr.trim() || ":80",
    tls_addr: tlsAddr.trim() || ":443",
  });

  const run = async (key: string, fn: () => Promise<void>) => {
    setBusy(key);
    try {
      await fn();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "操作失败");
    } finally {
      setBusy(null);
    }
  };

  const save = () =>
    run("save", async () => {
      if (mode === "acme" && (!email.trim() || !identifiers.trim())) {
        toast.error("ACME 模式需要填写联系邮箱与至少一个 IP/域名");
        return;
      }
      const r = await api.saveTLSSettings({
        mode,
        // 未编辑时不提交地址：后端保留原值，避免把“正在监听的地址”当成新地址试绑。
        https_addr: addrTouched ? addr.trim() : "",
        hsts,
        firewall_auto: firewallAuto,
        self_signed_sans: splitList(sansText),
        acme: acmeConfig(),
      });
      toast.success(r.needs_restart ? "已保存，需重启服务后生效" : "已保存");
      await load(true);
    });

  const regenerate = () =>
    run("self", async () => {
      const r = await api.tlsSelfSigned(splitList(sansText));
      toast.success(r.needs_restart ? "已生成自签证书，重启服务后生效" : "已生成新的自签证书");
      await load(true);
    });

  // 「启用 HTTPS（自签）」向导：显式生成自签证书并把 tls_mode 写成 selfsigned。
  // 默认部署保持纯 HTTP，只有点了这个按钮（或下面的导入/ACME 流程）才启用。
  const enableSelfSigned = () =>
    run("enable", async () => {
      const r = await api.tlsSelfSigned(splitList(sansText));
      toast.success(r.needs_restart ? "已启用 HTTPS（自签），重启服务后生效" : "已启用 HTTPS（自签）");
      await load(true);
    });

  const doImport = () =>
    run("import", async () => {
      if (!certPEM.trim() || !keyPEM.trim()) {
        toast.error("请粘贴证书与私钥 PEM");
        return;
      }
      const r = await api.tlsImport(certPEM, keyPEM);
      setCertPEM("");
      setKeyPEM("");
      toast.success(r.needs_restart ? "证书已导入，重启服务后生效" : "证书已导入并生效");
      await load(true);
    });

  const issue = () =>
    run("acme", async () => {
      if (!email.trim() || !identifiers.trim()) {
        toast.error("请填写邮箱与至少一个 IP/域名");
        return;
      }
      const r = await api.tlsIssueACME(acmeConfig());
      pollFails.current = 0;
      pollStart.current = Date.now();
      setPolling(true);
      toast.success(r.needs_restart ? "已开始申请；成功后需重启服务启用 HTTPS" : "已开始申请，进度会在下方刷新");
      await load(true);
    });

  const cancelACME = () =>
    run("cancel", async () => {
      await api.tlsCancelACME();
      toast.success("已取消本次申请");
      await load(true);
    });

  // 重启后进程会监听新地址/新协议：盲 reload 旧 URL 必然打不开（改端口、关 HTTPS
  // 尤其明显）。按保存后的模式与地址算出目标 URL，重启后跳过去。
  const nextURL = () => {
    if (typeof window === "undefined") return "";
    const scheme = mode === "off" ? "http" : "https";
    const raw = (addrTouched ? addr : info?.listen_addr || addr).trim();
    const m = /:(\d+)\s*$/.exec(raw);
    const port = m ? m[1] : "";
    const def = scheme === "https" ? "443" : "80";
    const portPart = port && port !== def ? `:${port}` : "";
    return `${scheme}://${window.location.hostname}${portPart}${window.location.pathname}${window.location.search}`;
  };

  const restart = () =>
    run("restart", async () => {
      const target = nextURL();
      await api.tlsRestart();
      toast.success(`服务正在重启，稍后跳转到 ${target || "新地址"}…`);
      setTimeout(() => {
        if (target && target !== window.location.href) window.location.replace(target);
        else window.location.reload();
      }, 8000);
    });

  const st = info?.status;
  const cert = st?.cert;
  const left = daysLeft(cert?.not_after);
  const active = !!info?.active;
  const activeAddr = info?.active_addr || "";
  // staging 证书浏览器不信任，必须显式标出——否则用户会以为“申请成功了但打不开”。
  const stagingCert = !!cert && /STAGING/i.test(cert.issuer || "");
  // 重启与否看“已保存的模式”，不看本地草稿：点向导按钮只是切换到 import/acme
  // 编辑态，尚未落库时不应提示“待重启”。
  const savedMode = st?.mode ?? mode;
  const needsRestart =
    !!info &&
    ((!active && savedMode !== "off") ||
      (active && savedMode === "off") ||
      (active && !!info.addr_saved && !!activeAddr && addr.trim() !== activeAddr));

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2 text-base">
          <LockIcon className="size-4" />
          HTTPS 与证书
          {st && (
            <Badge variant={active ? "default" : "secondary"} className="ml-1">
              {active ? "HTTPS 已启用" : "当前为 HTTP"}
            </Badge>
          )}
          {needsRestart && (
            <Badge variant="outline" className="border-amber-500/60 text-amber-600">
              待重启生效
            </Badge>
          )}
        </CardTitle>
        <CardDescription>
          默认<b>不启用 HTTPS</b>（纯 HTTP，下方可显式开启）。启用后 HTTPS 与 HTTP 共用同一端口（默认 8787）：远程
          http:// 访问会被 301 到 https://，本机回环明文直通，不占用 80 端口。公网 IP 可申请 Let&apos;s Encrypt 免费 IP
          证书；内网用自签；域名可导入现有证书或走 ACME。
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {loading && (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2Icon className="size-4 animate-spin" /> 读取中…
          </div>
        )}

        {!st && !loading && loadError && (
          <div className="flex flex-wrap items-center gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm">
            <TriangleAlertIcon className="size-4 shrink-0 text-destructive" />
            <span className="min-w-0 flex-1 break-all">{loadError}</span>
            <Button variant="outline" size="sm" onClick={() => void load()}>
              重试
            </Button>
          </div>
        )}

        {st && (
          <>
            {st.last_error && (
              <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-sm">
                <TriangleAlertIcon className="mt-0.5 size-4 shrink-0 text-amber-600" />
                <span className="break-all">{st.last_error}</span>
              </div>
            )}

            {/* 默认 pure HTTP：给出明确的显式启用入口（自签一键 / 导入 / ACME）。
                只有点击动作才会写 tls_mode 并提示重启。 */}
            {!active && mode === "off" && (
              <div className="flex flex-col gap-3 rounded-md border border-emerald-500/40 bg-emerald-500/5 p-3">
                <div className="flex items-start gap-2">
                  <ShieldCheckIcon className="mt-0.5 size-4 shrink-0 text-emerald-600" />
                  <div className="min-w-0 flex-1">
                    <div className="text-sm font-medium">当前为明文 HTTP（默认）</div>
                    <div className="text-xs text-muted-foreground">
                      HTTPS 需要显式启用：自签会立即生成证书并写入设置，重启服务后生效。内网/测试用自签；
                      公网域名可导入现有证书或走 Let&apos;s Encrypt。
                    </div>
                  </div>
                </div>
                <div className="flex flex-wrap items-center gap-2">
                  <Button size="sm" onClick={enableSelfSigned} disabled={busy !== null || st.acme.running}>
                    {busy === "enable" ? (
                      <Loader2Icon className="mr-1 size-4 animate-spin" />
                    ) : (
                      <LockIcon className="mr-1 size-4" />
                    )}
                    启用 HTTPS（自签）
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setMode("import")}
                    disabled={busy !== null || st.acme.running}
                  >
                    导入已有证书
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setMode("acme")}
                    disabled={busy !== null || st.acme.running}
                  >
                    Let&apos;s Encrypt
                  </Button>
                </div>
              </div>
            )}

            <div className="grid gap-3 text-sm sm:grid-cols-2">
              <div className="rounded-md border border-border/60 p-3">
                <div className="mb-1 font-medium">当前证书</div>
                {cert ? (
                  <div className="space-y-1 text-muted-foreground">
                    <div className="flex items-center gap-2">
                      <ShieldCheckIcon className="size-4 text-emerald-600" />
                      <span>{SOURCE_LABEL[cert.source] || cert.source}</span>
                      {stagingCert && <Badge variant="destructive">测试证书（浏览器不受信任）</Badge>}
                      {left !== null && !stagingCert && (
                        <Badge variant={left < 7 ? "destructive" : "secondary"}>
                          {left < 0 ? `已过期 ${Math.abs(left)} 天` : `${left} 天后到期`}
                        </Badge>
                      )}
                    </div>
                    <div className="break-all">颁发者：{cert.issuer}</div>
                    <div className="break-all">SAN：{cert.sans.join(", ") || "—"}</div>
                    <div>到期：{fmtDate(cert.not_after)}</div>
                  </div>
                ) : (
                  <div className="text-muted-foreground">尚无证书（关闭模式或尚未生成）</div>
                )}
                {st.ca_available && (
                  <Button
                    variant="outline"
                    size="sm"
                    className="mt-2"
                    disabled={busy !== null}
                    onClick={() => void run("ca", () => api.downloadTLSCA())}
                  >
                    <DownloadIcon className="mr-1 size-4" /> 下载本地 CA（供客户端信任）
                  </Button>
                )}
              </div>

              <div className="rounded-md border border-border/60 p-3">
                <div className="mb-1 font-medium">访问地址</div>
                <div className="space-y-1 text-muted-foreground">
                  {(info?.urls || []).slice(0, 4).map((u) => (
                    <div key={u} className="break-all font-mono text-xs">
                      {u}
                    </div>
                  ))}
                  {(!info?.urls || info.urls.length === 0) && <div>—</div>}
                  {activeAddr && (
                    <div className="pt-1 text-xs">
                      当前监听：<span className="font-mono">{activeAddr}</span>
                    </div>
                  )}
                </div>
                <div className="mt-2 text-xs text-muted-foreground">
                  防火墙：
                  {info?.firewall?.available
                    ? `${info.firewall.backend}${info.firewall.active ? "（已启用）" : "（未启用）"}`
                    : info?.firewall?.error || "宿主助手不可用"}
                  {info?.firewall?.hint ? ` · ${info.firewall.hint}` : ""}
                </div>
              </div>
            </div>

            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="tls-mode">证书模式</Label>
                <Select
                  value={mode}
                  onValueChange={(v) => setMode(v as TLSMode)}
                  disabled={busy !== null || st.acme.running}
                >
                  <SelectTrigger id="tls-mode" className="w-full min-w-0">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {(Object.keys(MODE_LABEL) as TLSMode[]).map((m) => (
                      <SelectItem key={m} value={m}>
                        {MODE_LABEL[m]}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="tls-addr">HTTPS 监听地址</Label>
                <Input
                  id="tls-addr"
                  value={addr}
                  onChange={(e) => {
                    setAddr(e.target.value);
                    setAddrTouched(true);
                  }}
                  placeholder=":8787"
                  disabled={busy !== null || st.acme.running || mode === "off"}
                />
                <p className="text-xs text-muted-foreground">
                  {mode === "off"
                    ? "关闭 HTTPS 时沿用启动参数 -addr 的端口。"
                    : "端口改完需重启服务；保存后优先于启动参数 -addr。"}
                </p>
              </div>
            </div>

            <div className="flex flex-col gap-3 rounded-md border border-border/60 p-3">
              <div className="flex items-center justify-between gap-3">
                <div>
                  <div className="text-sm font-medium">HSTS</div>
                  <div className="text-xs text-muted-foreground">
                    强制浏览器后续只用 HTTPS。自签证书建议关闭，否则证书告警无法跳过。
                  </div>
                </div>
                <Switch
                  id="tls-hsts"
                  aria-label="启用 HSTS"
                  checked={hsts}
                  onCheckedChange={setHsts}
                  disabled={busy !== null || st.acme.running}
                />
              </div>
              <div className="flex items-center justify-between gap-3">
                <div>
                  <div className="text-sm font-medium">ACME 期间自动放行防火墙</div>
                  <div className="text-xs text-muted-foreground">
                    仅在 80/443 原本未放行时临时添加，申请结束立即撤销；原本已放行则不动。云安全组需自行放行。
                  </div>
                </div>
                <Switch
                  id="tls-fw"
                  aria-label="ACME 期间自动放行防火墙"
                  checked={firewallAuto}
                  onCheckedChange={setFirewallAuto}
                  disabled={busy !== null || st.acme.running}
                />
              </div>
            </div>

            {mode === "selfsigned" && (
              <div className="space-y-2 rounded-md border border-border/60 p-3">
                <div className="text-sm font-medium">自签证书 SAN</div>
                <Textarea
                  value={sansText}
                  onChange={(e) => setSansText(e.target.value)}
                  rows={3}
                  placeholder={"留空自动包含主机名、127.0.0.1、::1 与本机网卡 IP；每行一个额外域名/IP"}
                  disabled={busy !== null}
                />
                <Button variant="outline" size="sm" onClick={regenerate} disabled={busy !== null}>
                  {busy === "self" ? (
                    <Loader2Icon className="mr-1 size-4 animate-spin" />
                  ) : (
                    <RefreshCwIcon className="mr-1 size-4" />
                  )}
                  重新生成自签证书
                </Button>
              </div>
            )}

            {mode === "import" && (
              <div className="space-y-2 rounded-md border border-border/60 p-3">
                <div className="text-sm font-medium">导入证书（PEM，未加密私钥）</div>
                <Textarea
                  value={certPEM}
                  onChange={(e) => setCertPEM(e.target.value)}
                  rows={4}
                  placeholder="-----BEGIN CERTIFICATE----- …（含中间证书时按 服务器→中间 顺序粘贴）"
                  disabled={busy !== null}
                />
                <Textarea
                  value={keyPEM}
                  onChange={(e) => setKeyPEM(e.target.value)}
                  rows={4}
                  placeholder="-----BEGIN PRIVATE KEY----- …"
                  disabled={busy !== null}
                />
                <Button variant="outline" size="sm" onClick={doImport} disabled={busy !== null}>
                  {busy === "import" ? (
                    <Loader2Icon className="mr-1 size-4 animate-spin" />
                  ) : (
                    <KeyRoundIcon className="mr-1 size-4" />
                  )}
                  校验并导入
                </Button>
              </div>
            )}

            {mode === "acme" && (
              <div className="space-y-3 rounded-md border border-border/60 p-3">
                <div className="text-sm font-medium">Let's Encrypt 自动申请</div>
                <div className="grid gap-3 sm:grid-cols-2">
                  <div className="space-y-1.5">
                    <Label htmlFor="tls-ids">IP / 域名（每行一个）</Label>
                    <Textarea
                      id="tls-ids"
                      value={identifiers}
                      onChange={(e) => setIdentifiers(e.target.value)}
                      rows={3}
                      placeholder={"203.0.113.10\npanel.example.com"}
                      disabled={busy !== null || st.acme.running}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="tls-email">联系邮箱</Label>
                    <Input
                      id="tls-email"
                      value={email}
                      onChange={(e) => setEmail(e.target.value)}
                      placeholder="ops@example.com"
                      disabled={busy !== null || st.acme.running}
                    />
                    <Label htmlFor="tls-challenge" className="pt-1">
                      验证方式
                    </Label>
                    <Select value={challenge} onValueChange={setChallenge} disabled={busy !== null || st.acme.running}>
                      <SelectTrigger id="tls-challenge" className="w-full min-w-0">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="http-01">HTTP-01（需要 80 端口）</SelectItem>
                        <SelectItem value="tls-alpn-01">TLS-ALPN-01（需要 443 端口）</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                </div>
                <div className="grid gap-3 sm:grid-cols-2">
                  <div className="space-y-1.5">
                    <Label htmlFor="tls-http-addr">HTTP-01 监听地址</Label>
                    <Input
                      id="tls-http-addr"
                      value={httpAddr}
                      onChange={(e) => setHttpAddr(e.target.value)}
                      placeholder=":80"
                      disabled={busy !== null || st.acme.running}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="tls-tls-addr">TLS-ALPN-01 监听地址</Label>
                    <Input
                      id="tls-tls-addr"
                      value={tlsAddr}
                      onChange={(e) => setTlsAddr(e.target.value)}
                      placeholder=":443"
                      disabled={busy !== null || st.acme.running}
                    />
                  </div>
                </div>
                <p className="text-xs text-muted-foreground">
                  80/443 被 nginx/Apache 占用时 ACME
                  必然失败；要么先让出端口，要么改这里的监听地址并由前置代理转发回源。
                </p>
                <div className="flex items-center justify-between gap-3">
                  <div className="text-xs text-muted-foreground">
                    IP 证书有效期约 6 天，程序在到期前一半自动续期；同一组标识 7 天内最多 5 次，调试请用 staging。
                  </div>
                  <div className="flex items-center gap-2">
                    <Label htmlFor="tls-staging" className="text-xs">
                      使用 staging
                    </Label>
                    <Switch
                      id="tls-staging"
                      checked={staging}
                      onCheckedChange={setStaging}
                      disabled={busy !== null || st.acme.running}
                    />
                  </div>
                </div>
                <div className="flex flex-wrap items-center gap-2">
                  <Button size="sm" onClick={issue} disabled={busy !== null || st.acme.running}>
                    {st.acme.running || busy === "acme" ? (
                      <Loader2Icon className="mr-1 size-4 animate-spin" />
                    ) : (
                      <ShieldCheckIcon className="mr-1 size-4" />
                    )}
                    {st.acme.running ? "申请中…" : "申请 / 立即续期"}
                  </Button>
                  {st.acme.running && (
                    <Button variant="outline" size="sm" onClick={cancelACME} disabled={busy !== null}>
                      取消
                    </Button>
                  )}
                  {st.acme.next_renew && !st.acme.next_renew.startsWith("0001-") && (
                    <span className="text-xs text-muted-foreground">下次续期：{fmtDate(st.acme.next_renew)}</span>
                  )}
                </div>
                {st.acme.last_error && (
                  <div className="break-all text-xs text-destructive">最近一次 ACME 错误：{st.acme.last_error}</div>
                )}
              </div>
            )}

            <div className="flex flex-wrap items-center gap-2 border-t pt-3">
              <Button onClick={save} disabled={busy !== null || st.acme.running}>
                {busy === "save" ? (
                  <Loader2Icon className="mr-1 size-4 animate-spin" />
                ) : (
                  <CheckCircle2Icon className="mr-1 size-4" />
                )}
                保存设置
              </Button>
              {(needsRestart || (savedMode !== "off" && !active)) && (
                <Button variant="outline" onClick={restart} disabled={busy !== null || st.acme.running}>
                  {busy === "restart" ? (
                    <Loader2Icon className="mr-1 size-4 animate-spin" />
                  ) : (
                    <RefreshCwIcon className="mr-1 size-4" />
                  )}
                  重启服务以生效
                </Button>
              )}
              <span className="text-xs text-muted-foreground">私钥仅保存在服务器 data/tls，接口从不返回私钥内容。</span>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
