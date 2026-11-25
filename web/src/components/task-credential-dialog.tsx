"use client";

import * as React from "react";

import { Loader2Icon, SaveIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { TaskCredential, TaskCredentialInput } from "@/lib/types";

type Form = {
  role: string;
  enabled: boolean;
  authMode: "form" | "json" | "basic" | "bearer" | "cookie";
  loginURL: string;
  method: "POST" | "GET";
  contentType: "json" | "form";
  username: string;
  password: string;
  token: string;
  totpSecret: string;
  usernameField: string;
  passwordField: string;
  successStatus: string;
  successContains: string;
  extraFields: string;
  headers: string;
};

const EMPTY: Form = {
  role: "",
  enabled: true,
  authMode: "form",
  loginURL: "",
  method: "POST",
  contentType: "json",
  username: "",
  password: "",
  token: "",
  totpSecret: "",
  usernameField: "username",
  passwordField: "password",
  successStatus: "",
  successContains: "",
  extraFields: "",
  headers: "",
};

function fromCredential(c: TaskCredential | null): Form {
  if (!c) return EMPTY;
  return {
    role: c.role,
    enabled: c.enabled,
    authMode: (c.auth_mode || "form") as Form["authMode"],
    loginURL: c.login_url || "",
    method: (c.method === "GET" ? "GET" : "POST") as Form["method"],
    contentType: (c.content_type === "form" ? "form" : "json") as Form["contentType"],
    username: c.username || "",
    password: "",
    token: "",
    totpSecret: "",
    usernameField: c.username_field || "username",
    passwordField: c.password_field || "password",
    successStatus: c.success_status ? String(c.success_status) : "",
    successContains: c.success_contains || "",
    extraFields: Object.entries(c.extra_fields ?? {}).map(([k, v]) => `${k}=${v}`).join("\n"),
    headers: Object.entries(c.headers ?? {}).map(([k, v]) => `${k}: ${v}`).join("\n"),
  };
}

function parsePairs(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    const i = trimmed.indexOf("=");
    if (i <= 0) continue;
    out[trimmed.slice(0, i).trim()] = trimmed.slice(i + 1);
  }
  return out;
}

function parseHeaders(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split(/\r?\n/)) {
    const i = line.indexOf(":");
    if (i <= 0) continue;
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return out;
}

export function TaskCredentialDialog({
  open,
  taskId,
  credential,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  taskId: string;
  credential: TaskCredential | null;
  onOpenChange: (open: boolean) => void;
  onSaved: (c: TaskCredential) => void;
}) {
  const [form, setForm] = React.useState<Form>(EMPTY);
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (open) setForm(fromCredential(credential));
  }, [open, credential]);

  const set = <K extends keyof Form>(key: K, value: Form[K]) => setForm((f) => ({ ...f, [key]: value }));

  async function save() {
    if (!form.role.trim()) {
      toast.error("角色名不能为空");
      return;
    }
    if (!form.loginURL.trim()) {
      toast.error("登录地址不能为空");
      return;
    }
    const status = form.successStatus.trim() ? Number(form.successStatus) : 0;
    if (!Number.isFinite(status) || status < 0 || status > 599) {
      toast.error("成功状态码需在 0-599 之间");
      return;
    }
    const config: TaskCredentialInput["config"] = {
      enabled: form.enabled,
      login_url: form.loginURL.trim(),
      method: form.method,
      content_type: form.contentType,
      auth_mode: form.authMode,
      username: form.username.trim(),
      password: form.password || undefined,
      token: form.token || undefined,
      totp_secret: form.totpSecret || undefined,
      username_field: form.usernameField.trim() || "username",
      password_field: form.passwordField.trim() || "password",
      success_status: status || undefined,
      success_contains: form.successContains.trim() || undefined,
      extra_fields: parsePairs(form.extraFields),
      headers: parseHeaders(form.headers),
    };
    setSaving(true);
    try {
      const input: TaskCredentialInput = { role: form.role.trim(), config };
      const saved = credential
        ? await api.updateTaskCredential(taskId, credential.id, input)
        : await api.createTaskCredential(taskId, input);
      toast.success(credential ? "凭据已更新" : "凭据已添加");
      onSaved(saved);
      onOpenChange(false);
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  const showUserPass = form.authMode === "form" || form.authMode === "json" || form.authMode === "basic";
  const showToken = form.authMode === "basic" || form.authMode === "bearer" || form.authMode === "cookie";

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-[640px]">
        <DialogHeader>
          <DialogTitle>{credential ? `编辑凭据 · ${credential.role}` : "添加多角色凭据"}</DialogTitle>
          <DialogDescription>
            密码、Token、TOTP 只写入数据库，API 不回显；worker 只看到凭据 ID、角色和健康状态。
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-3 sm:grid-cols-2">
          <label className="flex flex-col gap-1.5 text-sm">
            <span className="text-muted-foreground text-xs">角色名（如 admin / user_a）</span>
            <Input value={form.role} onChange={(e) => set("role", e.target.value)} placeholder="user_a" />
          </label>
          <label className="flex flex-col gap-1.5 text-sm">
            <span className="text-muted-foreground text-xs">认证模式</span>
            <NativeSelect value={form.authMode} onChange={(e) => set("authMode", e.target.value as Form["authMode"])}>
              <NativeSelectOption value="form">表单（form）</NativeSelectOption>
              <NativeSelectOption value="json">JSON</NativeSelectOption>
              <NativeSelectOption value="basic">Basic</NativeSelectOption>
              <NativeSelectOption value="bearer">Bearer Token</NativeSelectOption>
              <NativeSelectOption value="cookie">Cookie</NativeSelectOption>
            </NativeSelect>
          </label>
          <label className="flex flex-col gap-1.5 text-sm sm:col-span-2">
            <span className="text-muted-foreground text-xs">登录地址</span>
            <Input value={form.loginURL} onChange={(e) => set("loginURL", e.target.value)} placeholder="https://target.example.com/api/login" />
          </label>
          <label className="flex flex-col gap-1.5 text-sm">
            <span className="text-muted-foreground text-xs">请求方法</span>
            <NativeSelect value={form.method} onChange={(e) => set("method", e.target.value as Form["method"])}>
              <NativeSelectOption value="POST">POST</NativeSelectOption>
              <NativeSelectOption value="GET">GET</NativeSelectOption>
            </NativeSelect>
          </label>
          <label className="flex flex-col gap-1.5 text-sm">
            <span className="text-muted-foreground text-xs">请求体格式</span>
            <NativeSelect value={form.contentType} onChange={(e) => set("contentType", e.target.value as Form["contentType"])}>
              <NativeSelectOption value="json">JSON</NativeSelectOption>
              <NativeSelectOption value="form">form-urlencoded</NativeSelectOption>
            </NativeSelect>
          </label>

          {showUserPass ? (
            <>
              <label className="flex flex-col gap-1.5 text-sm">
                <span className="text-muted-foreground text-xs">用户名</span>
                <Input value={form.username} onChange={(e) => set("username", e.target.value)} autoComplete="off" />
              </label>
              <label className="flex flex-col gap-1.5 text-sm">
                <span className="text-muted-foreground text-xs">密码{credential ? "（留空=保持不变）" : ""}</span>
                <Input type="password" value={form.password} onChange={(e) => set("password", e.target.value)} autoComplete="new-password" />
              </label>
              <label className="flex flex-col gap-1.5 text-sm">
                <span className="text-muted-foreground text-xs">用户名字段</span>
                <Input value={form.usernameField} onChange={(e) => set("usernameField", e.target.value)} />
              </label>
              <label className="flex flex-col gap-1.5 text-sm">
                <span className="text-muted-foreground text-xs">密码字段</span>
                <Input value={form.passwordField} onChange={(e) => set("passwordField", e.target.value)} />
              </label>
            </>
          ) : null}

          {showToken ? (
            <label className="flex flex-col gap-1.5 text-sm sm:col-span-2">
              <span className="text-muted-foreground text-xs">Token{credential ? "（留空=保持不变）" : ""}</span>
              <Input type="password" value={form.token} onChange={(e) => set("token", e.target.value)} autoComplete="new-password" />
            </label>
          ) : null}

          <label className="flex flex-col gap-1.5 text-sm">
            <span className="text-muted-foreground text-xs">TOTP 密钥（可选）{credential ? "，留空=保持不变" : ""}</span>
            <Input type="password" value={form.totpSecret} onChange={(e) => set("totpSecret", e.target.value)} autoComplete="off" />
          </label>
          <label className="flex flex-col gap-1.5 text-sm">
            <span className="text-muted-foreground text-xs">成功状态码（0=2xx/3xx）</span>
            <Input type="number" min={0} max={599} value={form.successStatus} onChange={(e) => set("successStatus", e.target.value)} />
          </label>
          <label className="flex flex-col gap-1.5 text-sm sm:col-span-2">
            <span className="text-muted-foreground text-xs">响应包含（可选）</span>
            <Input value={form.successContains} onChange={(e) => set("successContains", e.target.value)} placeholder="例如 token / dashboard" />
          </label>
          <label className="flex flex-col gap-1.5 text-sm">
            <span className="text-muted-foreground text-xs">附加字段（每行 k=v）</span>
            <Textarea rows={3} value={form.extraFields} onChange={(e) => set("extraFields", e.target.value)} placeholder="remember=true" />
          </label>
          <label className="flex flex-col gap-1.5 text-sm">
            <span className="text-muted-foreground text-xs">附加请求头（每行 Key: Value）</span>
            <Textarea rows={3} value={form.headers} onChange={(e) => set("headers", e.target.value)} placeholder="X-Requested-With: XMLHttpRequest" />
          </label>
          <label className="flex items-center gap-2 text-sm sm:col-span-2">
            <Switch checked={form.enabled} onCheckedChange={(v) => set("enabled", !!v)} />
            启用该凭据（关闭后 worker 不会看到）
          </label>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={saving}>
            取消
          </Button>
          <Button onClick={() => void save()} disabled={saving}>
            {saving ? <Loader2Icon className="animate-spin" /> : <SaveIcon />} 保存
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
