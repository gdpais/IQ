type Metric = {
  name: string;
  value: string;
};

const metrics: Metric[] = [
  { name: "Open Incidents", value: "--" },
  { name: "MTTA", value: "--" },
  { name: "MTTR", value: "--" },
  { name: "SLA At Risk", value: "--" }
];

export function App() {
  return (
    <main className="layout">
      <header className="topbar">
        <h1>IncidentIQ</h1>
        <p>Real-time incident reporting for SRE operations</p>
      </header>

      <section className="metrics-grid">
        {metrics.map((metric) => (
          <article key={metric.name} className="metric-card">
            <h2>{metric.name}</h2>
            <p>{metric.value}</p>
          </article>
        ))}
      </section>

      <section className="panel">
        <h2>Incident Register</h2>
        <p>API integration in progress. This screen will list tracked incidents.</p>
      </section>
    </main>
  );
}
