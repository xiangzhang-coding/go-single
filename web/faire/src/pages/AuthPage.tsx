import { useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useMutation } from "@tanstack/react-query";

import { authApi } from "../api/endpoints";
import { getApiErrorMessage } from "../api/client";
import { Icon, Button, Spinner } from "../components/ui";
import { startSession } from "../lib/session";

export function AuthPage({ mode }: { mode: "login" | "register" }) {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [formError, setFormError] = useState("");
  const isLogin = mode === "login";
  const registered = searchParams.get("registered") === "1";

  const mutation = useMutation({
    mutationFn: async () => {
      if (!isLogin && password !== confirmPassword) {
        throw new Error("两次输入的密码不一致");
      }
      if (isLogin) {
        return { kind: "login" as const, result: await authApi.login(username, password) };
      }
      return { kind: "register" as const, result: await authApi.register(username, password) };
    },
    onSuccess: ({ kind, result }) => {
      if (kind === "login") {
        startSession(result.token, result.user);
        const returnTo = searchParams.get("returnTo");
        navigate(returnTo?.startsWith("/") ? returnTo : "/", { replace: true });
        return;
      }
      navigate("/login?registered=1", { replace: true });
    },
    onError: (error) => setFormError(getApiErrorMessage(error)),
  });

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setFormError("");
    mutation.mutate();
  }

  return (
    <div className="auth-page">
      <div className="auth-split">
        <section className="auth-intro">
          <Link to="/" className="brand-mark">
            FAIRE<span>/</span>
          </Link>
          <div className="auth-intro-copy">
            <p className="eyebrow text-smoke">FAIRE / 会员目录</p>
            <h1 className="font-nantes text-5xl leading-[1.08] sm:text-6xl">留下真正想买的。</h1>
            <p className="mt-6 max-w-md text-base leading-7 text-charcoal">
              登录后，购物车、地址和订单会留在你的目录里。没有广告，也不替你做选择。
            </p>
          </div>
          <div className="auth-intro-note">
            <span className="accent-rule" aria-hidden="true" />
            <span>浏览公开商品，登录后完成购买。</span>
          </div>
        </section>

        <section className="auth-form-panel">
          <div className="auth-form-wrap">
            <div className="flex items-start justify-between gap-4">
              <div>
                <p className="eyebrow text-smoke">{isLogin ? "欢迎回来" : "建立你的目录"}</p>
                <h2 className="mt-3 font-nantes text-4xl">{isLogin ? "登录 FAIRE" : "注册账号"}</h2>
              </div>
              <Link to="/" className="icon-button" aria-label="返回首页">
                <Icon name="close" size={18} />
              </Link>
            </div>

            {registered && isLogin && (
              <div className="notice notice-success mt-8">账号已创建，现在可以登录了。</div>
            )}

            <form onSubmit={submit} className="mt-8 space-y-5">
              <label className="form-label">
                <span>用户名</span>
                <input
                  className="form-control"
                  value={username}
                  onChange={(event) => setUsername(event.target.value)}
                  autoComplete="username"
                  required
                  minLength={3}
                  maxLength={32}
                  placeholder="例如 studio_user"
                />
              </label>
              <label className="form-label">
                <span>密码</span>
                <input
                  className="form-control"
                  type="password"
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                  autoComplete={isLogin ? "current-password" : "new-password"}
                  required
                  minLength={6}
                  placeholder="至少 6 位字符"
                />
              </label>
              {!isLogin && (
                <label className="form-label">
                  <span>再次输入密码</span>
                  <input
                    className="form-control"
                    type="password"
                    value={confirmPassword}
                    onChange={(event) => setConfirmPassword(event.target.value)}
                    autoComplete="new-password"
                    required
                    minLength={6}
                    placeholder="确认密码"
                  />
                </label>
              )}

              {formError && <div className="notice notice-error">{formError}</div>}

              <Button type="submit" className="w-full justify-center" disabled={mutation.isPending}>
                {mutation.isPending ? <Spinner label="请稍候" /> : <>{isLogin ? "登录并继续" : "创建账号"}<Icon name="arrow-right" size={17} /></>}
              </Button>
            </form>

            <p className="mt-8 text-center text-sm text-smoke">
              {isLogin ? "还没有账号？" : "已经有账号？"}{" "}
              <Link to={isLogin ? "/register" : "/login"} className="underline underline-offset-4 text-ink-black">
                {isLogin ? "注册一个" : "返回登录"}
              </Link>
            </p>
          </div>
        </section>
      </div>
    </div>
  );
}
