import React, { Suspense, lazy } from "react";
import { createRoot } from "react-dom/client";
import { ErrorBoundary } from "./error-boundary";
import "./app.css";

// Visitors to /home and /news download only the public pages; the console
// (including every administration screen) stays at "/" in its own chunk.
const PublicSite = lazy(() => import("./public-site"));
const Console = lazy(() => import("./console"));
const publicPage = /^\/(home|news)(\/|$)/.test(location.pathname);

createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <ErrorBoundary>
      <Suspense
        fallback={
          <div className="empty" role="status">
            正在读取…
          </div>
        }
      >
        {publicPage ? <PublicSite /> : <Console />}
      </Suspense>
    </ErrorBoundary>
  </React.StrictMode>,
);
