export type NavigationGroup = { title: string; items: string[] };

// Keep free deployment tools above paid services for every account type.
export function navigationGroups(
  admin: boolean,
  subscribed: boolean,
): NavigationGroup[] {
  return [
    { title: "开始使用", items: ["tutorials", "home"] },
    { title: "免费部署工具", items: ["deploy", "relay", "dd", "tasks"] },
    {
      title: admin ? "增值业务管理" : "增值服务",
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
