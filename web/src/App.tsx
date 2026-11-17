import { useEffect, useState } from "react";
import { FleetView } from "./components/FleetView";
import { RunDetail } from "./components/RunDetail";

/** Hash routing: "#/" is the fleet, "#/runs/<id>" is one run. A link to a run survives a reload. */
function useRoute(): string | null {
  const read = () => {
    const match = window.location.hash.match(/^#\/runs\/([A-Za-z0-9]+)/);
    return match ? match[1] : null;
  };
  const [runId, setRunId] = useState<string | null>(read);
  useEffect(() => {
    const onChange = () => setRunId(read());
    window.addEventListener("hashchange", onChange);
    return () => window.removeEventListener("hashchange", onChange);
  }, []);
  return runId;
}

export function App() {
  const runId = useRoute();
  return (
    <div className="app">
      <header className="topbar">
        <a className="brand" href="#/">
          <svg viewBox="0 0 32 32" width="22" height="22" aria-hidden="true">
            <path d="M5 20h22l-4 6H9z" fill="currentColor" />
            <path d="M10 18V8h12v10" fill="none" stroke="currentColor" strokeWidth="2.2" />
            <path d="M16 8v10" stroke="currentColor" strokeWidth="2.2" />
          </svg>
          drydock
        </a>
        <span className="tagline">coding agents in isolated sandboxes</span>
      </header>
      {runId ? <RunDetail key={runId} id={runId} /> : <FleetView />}
    </div>
  );
}
