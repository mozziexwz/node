import React from "react";

// One render error should leave a recoverable message, not a blank page. This
// file stays free of shared UI imports so it ships in the small entry chunk.
export class ErrorBoundary extends React.Component<
  { children: React.ReactNode },
  { error: Error | null }
> {
  state = { error: null as Error | null };
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  componentDidCatch(error: Error, info: React.ErrorInfo) {
    console.error(error, info.componentStack);
  }
  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="card crash-card" role="alert">
        <h2>页面出错了</h2>
        <p className="muted mt8">
          {this.state.error.message || "发生了未知错误。"}
        </p>
        <button
          type="button"
          className="btn primary mt16"
          onClick={() => location.reload()}
        >
          刷新页面
        </button>
      </div>
    );
  }
}
