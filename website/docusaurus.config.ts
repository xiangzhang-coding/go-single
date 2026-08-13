import { themes as prismThemes } from "prism-react-renderer";
import type { Config } from "@docusaurus/types";
import type * as Preset from "@docusaurus/preset-classic";

const config: Config = {
  title: "Go 单体商城",
  tagline: "学习 Go 后端技术栈的模块化单体商城项目（在线商城 / 秒杀 / 后台 / 好友圈 / 即时通信）",
  favicon: "img/logo.svg",
  url: "https://go-single.pages.dev",
  baseUrl: "/",
  organizationName: "xiangzhang-coding",
  projectName: "go-single",
  i18n: {
    defaultLocale: "zh-CN",
    locales: ["zh-CN"],
  },
  onBrokenLinks: "throw",
  onBrokenMarkdownLinks: "warn",
  presets: [
    [
      "classic",
      {
        docs: {
          sidebarPath: "./sidebars.ts",
          editUrl: "https://github.com/xiangzhang-coding/go-single/tree/main/website/",
        },
        blog: false,
        theme: {
          customCss: "./src/css/custom.css",
        },
      } satisfies Preset.Options,
    ],
  ],
  themeConfig: {
    image: "img/logo.svg",
    colorMode: {
      defaultMode: "light",
      disableSwitch: false,
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: "Go 单体商城",
      logo: {
        alt: "Go 单体商城",
        src: "img/logo.svg",
      },
      items: [
        {
          type: "docSidebar",
          sidebarId: "docs",
          position: "left",
          label: "用户指南",
        },
        {
          to: "/docs/tech/modules",
          position: "left",
          label: "技术文档",
        },
        {
          href: "https://github.com/xiangzhang-coding/go-single",
          label: "GitHub",
          position: "right",
        },
      ],
    },
    footer: {
      style: "light",
      links: [
        {
          title: "用户指南",
          items: [
            { label: "快速开始", to: "/docs/user-guide/quickstart" },
            { label: "演示账号", to: "/docs/user-guide/demo-accounts" },
            { label: "功能向导", to: "/docs/user-guide/feature-guide" },
          ],
        },
        {
          title: "技术文档",
          items: [
            { label: "模块（modules）", to: "/docs/tech/modules" },
            { label: "领域（domains）", to: "/docs/tech/domains" },
          ],
        },
        {
          title: "更多",
          items: [
            { label: "GitHub 仓库", href: "https://github.com/xiangzhang-coding/go-single" },
            { label: "工程文档", href: "https://github.com/xiangzhang-coding/go-single/tree/main/docs" },
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} go-single`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
