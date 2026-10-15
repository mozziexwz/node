import React from "react";
import { House, Newspaper, LayoutDashboard } from "lucide-react";

export type SiteSection = "home" | "news" | "console";

const sections: [SiteSection, string, string, typeof House][] = [
  ["home", "/home", "主页", House],
  ["news", "/news", "文章", Newspaper],
  ["console", "/", "控制台", LayoutDashboard],
];

// Real links keep middle-click, copy-link and crawlers working; the public site
// passes onNavigate to switch between its own pages without a reload.
export function SiteSwitcher({
  current,
  onNavigate,
}: {
  current: SiteSection;
  onNavigate?: (
    href: string,
    event: React.MouseEvent<HTMLAnchorElement>,
  ) => void;
}) {
  return (
    <nav className="site-switcher" aria-label="站点切换">
      {sections.map(([key, href, label, Icon]) => (
        <a
          key={key}
          href={href}
          title={label}
          aria-label={label}
          aria-current={current === key ? "page" : undefined}
          className={current === key ? "active" : ""}
          onClick={(event) => {
            if (current === key) {
              event.preventDefault();
              return;
            }
            onNavigate?.(href, event);
          }}
        >
          <Icon size={15} aria-hidden="true" />
          <span>{label}</span>
        </a>
      ))}
    </nav>
  );
}
