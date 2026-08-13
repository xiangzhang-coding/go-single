import clsx from "clsx";
import Link from "@docusaurus/Link";
import useDocusaurusContext from "@docusaurus/useDocusaurusContext";
import Heading from "@theme/Heading";
import styles from "./index.module.css";

type Feature = {
  icon: string;
  title: string;
  description: string;
  to: string;
};

const features: Feature[] = [
  {
    icon: "🚀",
    title: "快速开始",
    description: "docker compose + go run + bun dev 三步本地启动全栈项目。",
    to: "/docs/user-guide/quickstart",
  },
  {
    icon: "🧭",
    title: "功能向导",
    description: "从注册登录到聊天，按真实流程走完商城、秒杀、好友圈与即时通信。",
    to: "/docs/user-guide/feature-guide",
  },
  {
    icon: "🧩",
    title: "技术文档 · 模块",
    description: "镜像 internal/ 的模块视图：数据模型 / 接口 / 时序（建设中）。",
    to: "/docs/tech/modules",
  },
  {
    icon: "🗂️",
    title: "技术文档 · 领域",
    description: "镜像 CONTEXT.md 的领域分组：模块 ↔ 领域映射薄视图（建设中）。",
    to: "/docs/tech/domains",
  },
];

export default function Home(): React.JSX.Element {
  const { siteConfig } = useDocusaurusContext();
  return (
    <main>
      <section className={clsx("hero hero--primary hero--landing")}>
        <div className="container">
          <h1 className="hero__title">{siteConfig.title}</h1>
          <p className="hero__subtitle">{siteConfig.tagline}</p>
          <div>
            <Link className="button button--secondary button--lg" to="/docs/user-guide/quickstart">
              快速开始
            </Link>
            <Link
              className="button button--outline button--lg margin-left--md"
              to="/docs/tech/modules"
            >
              技术文档
            </Link>
          </div>
        </div>
      </section>
      <section className={styles.features}>
        <div className="landing-features">
          {features.map((f) => (
            <Link key={f.title} className="landing-feature" to={f.to}>
              <div className="landing-feature__icon">{f.icon}</div>
              <h2 className="landing-feature__title">{f.title}</h2>
              <p className="landing-feature__desc">{f.description}</p>
            </Link>
          ))}
        </div>
      </section>
      <section style={{ padding: "0 0 3rem", textAlign: "center" }}>
        <Heading as="h2">工程决策权威源在 docs/</Heading>
        <p className="margin-vert--md">
          本网站只放摘要与链接；ADR / DESIGN / BACKLOG 以仓库
          <Link href="https://github.com/xiangzhang-coding/go-single/tree/main/docs"> docs/ </Link>
          为准。
        </p>
      </section>
    </main>
  );
}
