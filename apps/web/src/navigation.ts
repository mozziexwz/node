export type NavigationGroup = { title: string; items: string[] };

// A link elsewhere in the console (the member guide's latest articles) sets
// this before opening the article page, which then shows that article first.
export const openArticleKey = "msboost-open-article";

// Homepage links such as /#deploy open these after signing in.
export const freeTools = [
  "launcher",
  "local",
  "deploy",
  "relay",
  "codes",
  "dd",
  "tasks",
];

// Keep free deployment tools above paid services for every account type.
// Members see MSBOOST Local only once its download is published.
export function navigationGroups(
  admin: boolean,
  subscribed: boolean,
  localApp: boolean,
): NavigationGroup[] {
  return [
    { title: "开始使用", items: ["tutorials", "home"] },
    {
      title: "免费部署工具",
      items: freeTools.filter((k) => k !== "local" || admin || localApp),
    },
    {
      title: admin ? "捐赠权益管理" : "捐赠权益",
      items: admin
        ? ["agents", "routes", "rules", "plans", "cards", "orders", "payments"]
        : [
            ...(subscribed ? ["routes", "traffic", "status"] : []),
            "plans",
            "wallet",
            "orders",
          ],
    },
    ...(admin
      ? [
          {
            title: "平台管理",
            items: [
              "users",
              "executors",
              "relayAudit",
              "emailLog",
              "invitations",
              "backups",
              "settings",
              "release",
            ],
          },
        ]
      : []),
    { title: "账户与支持", items: ["account", "tickets"] },
  ];
}
