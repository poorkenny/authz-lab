import { Component, type ReactNode } from "react";

// ErrorBoundary keeps a rendering bug from blanking the whole page: it shows
// what went wrong and a way to recover instead.
export class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidCatch(error: Error) {
    console.error("[authz-lab] UI error:", error);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div className="app">
        <section className="card" role="alert">
          <h2>Something went wrong in the demo</h2>
          <p className="txt-bad mono small">{this.state.error.message}</p>
          <p>Your session is safe on the server. Reload to start the story again.</p>
          <button className="primary-btn" onClick={() => location.reload()}>
            Reload
          </button>
        </section>
      </div>
    );
  }
}
