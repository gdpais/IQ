import { FormEvent, ReactNode, useEffect, useMemo, useState } from "react";

const API_BASE_URL = (import.meta.env.VITE_API_BASE_URL ?? "http://localhost:8080").replace(/\/$/, "");
const API_AUTH_TOKEN = import.meta.env.VITE_API_AUTH_TOKEN ?? "";
const API_ROLE = import.meta.env.VITE_API_ROLE ?? "admin";
const API_ACTOR = import.meta.env.VITE_API_ACTOR ?? "operator-ui";

type View = "incidents" | "reports" | "integrations" | "templates";
type Status = "open" | "acknowledged" | "resolved";

type Incident = {
  id: string;
  title: string;
  summary: string;
  severity: string;
  service: string;
  environment: string;
  owner_team: string;
  status: Status;
  started_at: string;
  acknowledged_at?: string;
  resolved_at?: string;
  created_at: string;
  updated_at: string;
};

type IncidentEvent = {
  id: string;
  incident_id: string;
  type: string;
  actor: string;
  payload: Record<string, unknown>;
  created_at: string;
};

type Alert = {
  id: string;
  source: string;
  source_event_id: string;
  fingerprint: string;
  severity: string;
  service: string;
  environment: string;
  observed_at: string;
};

type JIRALink = {
  jira_issue_key: string;
  jira_issue_id?: string;
  updated_at: string;
};

type IncidentDetail = {
  incident: Incident;
  events: IncidentEvent[];
  alerts: Alert[];
  jira?: JIRALink;
};

type LiveMetrics = {
  incident_count: number;
  open_incident_count: number;
  acknowledged_incident_count: number;
  resolved_incident_count: number;
  alert_count: number;
  incidents_with_alerts: number;
  alert_to_incident_conversion: number;
  duplicate_suppression_count: number;
  reopen_count: number;
  reopen_rate: number;
  mttd_seconds?: number;
  mtta_seconds?: number;
  mttr_seconds?: number;
  sla_at_risk_count: number;
  sla_breach_count: number;
  counts_by_severity: Record<string, number>;
  counts_by_service: Record<string, number>;
  counts_by_environment: Record<string, number>;
  counts_by_team: Record<string, number>;
  counts_by_status: Record<string, number>;
};

type SREReport = LiveMetrics & {
  generated_at: string;
};

type DORAReport = {
  deployment_count: number;
  deployment_linked_incidents: number;
  change_failure_rate: number;
  time_to_restore_service_seconds?: number;
  generated_at: string;
};

type ExecutiveReport = {
  incident_count: number;
  downtime_seconds: number;
  sla_at_risk_count: number;
  sla_breach_count: number;
  severity_trends: Record<string, number>;
  service_reliability: Record<string, number>;
  business_impact_notes: string[];
  generated_at: string;
};

type IntegrationStatus = {
  name: string;
  enabled: boolean;
  configured: boolean;
  connected?: boolean;
  base_url?: string;
  project_key?: string;
  sender_display_name?: string;
  sender_upn?: string;
  tenant_id?: string;
};

type IntegrationEvent = {
  id: string;
  integration_name: string;
  type: string;
  status: string;
  payload: Record<string, unknown>;
  attempts: number;
  next_retry_at?: string;
  created_at: string;
  updated_at: string;
};

type DirectoryEntry = {
  id: string;
  display_name: string;
  description?: string;
  upn?: string;
};

type TeamsRouteRecipient = {
  id?: string;
  route_id?: string;
  type: "user" | "tag";
  teams_object_id: string;
  display_name: string;
  upn?: string;
};

type TeamsRoute = {
  id: string;
  name: string;
  enabled: boolean;
  team_id: string;
  team_name: string;
  channel_id: string;
  channel_name: string;
  owner_team?: string;
  service?: string;
  environment?: string;
  severity_min?: string;
  recipients: TeamsRouteRecipient[];
};

type TeamsRouteForm = {
  id?: string;
  name: string;
  enabled: boolean;
  team_id: string;
  team_name: string;
  channel_id: string;
  channel_name: string;
  owner_team: string;
  service: string;
  environment: string;
  severity_min: string;
  recipients: TeamsRouteRecipient[];
};

type Filters = {
  severity: string;
  service: string;
  status: string;
  owner: string;
  from: string;
  to: string;
};

type ReportFilters = {
  from: string;
  to: string;
  service: string;
  environment: string;
  team: string;
};

type CreateIncidentForm = {
  title: string;
  summary: string;
  severity: string;
  service: string;
  environment: string;
  owner_team: string;
};

const emptyIncidentForm: CreateIncidentForm = {
  title: "",
  summary: "",
  severity: "high",
  service: "",
  environment: "prod",
  owner_team: "sre"
};

export const emptyTeamsRouteForm: TeamsRouteForm = {
  name: "",
  enabled: true,
  team_id: "",
  team_name: "",
  channel_id: "",
  channel_name: "",
  owner_team: "",
  service: "",
  environment: "",
  severity_min: "",
  recipients: []
};

const defaultTemplate = `version: 1
defaults:
  project_key: OPS
  issue_type: Task
  title: "{{severity}} incident: {{service}}"
  description: "{{summary}}"
  labels:
    - incidentiq
    - "{{environment}}"
  priority:
    critical: Highest
    high: High
    medium: Medium
    low: Low
overrides:
  - scope:
      type: severity
      value: critical
    template:
      labels:
        - incidentiq
        - critical
      comments:
        - "Dashboard: {{dashboard_url}}"
`;

const defaultTemplateContext = `{
  "severity": "critical",
  "service": "checkout",
  "environment": "prod",
  "incident_id": "inc-preview",
  "started_at": "2026-06-02T12:00:00Z",
  "summary": "Checkout API error rate is elevated.",
  "alert_count": 3,
  "dashboard_url": "https://incidentiq.local/incidents/inc-preview"
}`;

function buildQuery(values: Record<string, string | undefined>) {
  const params = new URLSearchParams();
  Object.entries(values).forEach(([key, value]) => {
    if (value) {
      params.set(key, value);
    }
  });
  const query = params.toString();
  return query ? `?${query}` : "";
}

async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
	const response = await fetch(`${API_BASE_URL}${path}`, {
		...init,
		headers: apiHeaders(init?.headers)
	});
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(typeof body.error === "string" ? body.error : `HTTP ${response.status}`);
  }
	return response.json() as Promise<T>;
}

function apiHeaders(headers?: HeadersInit) {
	return {
		"Content-Type": "application/json",
		"X-Actor": API_ACTOR,
		"X-Role": API_ROLE,
		...(API_AUTH_TOKEN ? { Authorization: `Bearer ${API_AUTH_TOKEN}` } : {}),
		...(headers ?? {})
	};
}

function formatDate(value?: string) {
  if (!value) {
    return "-";
  }
  return new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit"
  }).format(new Date(value));
}

function formatDuration(seconds?: number) {
  if (seconds === undefined || seconds === null) {
    return "-";
  }
  if (seconds < 60) {
    return `${Math.round(seconds)}s`;
  }
  if (seconds < 3600) {
    return `${Math.round(seconds / 60)}m`;
  }
  return `${(seconds / 3600).toFixed(1)}h`;
}

function percent(value: number) {
  return `${(value * 100).toFixed(1)}%`;
}

export function routeToForm(route: TeamsRoute): TeamsRouteForm {
  return {
    id: route.id,
    name: route.name,
    enabled: route.enabled,
    team_id: route.team_id,
    team_name: route.team_name,
    channel_id: route.channel_id,
    channel_name: route.channel_name,
    owner_team: route.owner_team ?? "",
    service: route.service ?? "",
    environment: route.environment ?? "",
    severity_min: route.severity_min ?? "",
    recipients: route.recipients ?? []
  };
}

function severityRank(severity: string) {
  return { critical: 4, high: 3, medium: 2, low: 1 }[severity] ?? 0;
}

function statusRank(status: string) {
  return { open: 3, acknowledged: 2, resolved: 1 }[status] ?? 0;
}

function AppShell({
  active,
  setActive,
  children
}: {
  active: View;
  setActive: (view: View) => void;
  children: ReactNode;
}) {
  const tabs: Array<{ id: View; label: string }> = [
    { id: "incidents", label: "Incidents" },
    { id: "reports", label: "Reports" },
    { id: "integrations", label: "Integrations" },
    { id: "templates", label: "Templates" }
  ];

  return (
    <main className="app">
      <header className="app-header">
        <div>
          <h1>IncidentIQ</h1>
          <p>Production SRE incident operations</p>
        </div>
        <nav className="tabs" aria-label="Primary">
          {tabs.map((tab) => (
            <button
              key={tab.id}
              className={active === tab.id ? "tab active" : "tab"}
              type="button"
              onClick={() => setActive(tab.id)}
            >
              {tab.label}
            </button>
          ))}
        </nav>
      </header>
      {children}
    </main>
  );
}

export function App() {
  const [active, setActive] = useState<View>("incidents");

  return (
    <AppShell active={active} setActive={setActive}>
      {active === "incidents" && <IncidentWorkspace />}
      {active === "reports" && <ReportsWorkspace />}
      {active === "integrations" && <IntegrationsWorkspace />}
      {active === "templates" && <TemplatesWorkspace />}
    </AppShell>
  );
}

function IncidentWorkspace() {
  const [filters, setFilters] = useState<Filters>({
    severity: "",
    service: "",
    status: "",
    owner: "",
    from: "",
    to: ""
  });
  const [incidents, setIncidents] = useState<Incident[]>([]);
  const [selectedId, setSelectedId] = useState<string>("");
  const [detail, setDetail] = useState<IncidentDetail | null>(null);
  const [metrics, setMetrics] = useState<LiveMetrics | null>(null);
  const [sort, setSort] = useState<"started" | "severity" | "status">("started");
  const [form, setForm] = useState<CreateIncidentForm>(emptyIncidentForm);
  const [loading, setLoading] = useState(true);
  const [detailLoading, setDetailLoading] = useState(false);
  const [actionBusy, setActionBusy] = useState("");
  const [error, setError] = useState("");

  const loadIncidents = async (signal?: AbortSignal) => {
    setLoading(true);
    setError("");
    const query = buildQuery({
      severity: filters.severity,
      service: filters.service,
      status: filters.status,
      owner: filters.owner,
      started_from: filters.from,
      started_to: filters.to,
      limit: "100"
    });
    try {
      const [items, live] = await Promise.all([
        apiFetch<Incident[]>(`/incidents${query}`, { signal }),
        apiFetch<LiveMetrics>(`/metrics/live${query}`, { signal })
      ]);
      setIncidents(items);
      setMetrics(live);
      if (!selectedId && items.length > 0) {
        setSelectedId(items[0].id);
      }
    } catch (err) {
      if (!(err instanceof DOMException && err.name === "AbortError")) {
        setError(err instanceof Error ? err.message : "Unable to load incidents");
      }
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    const controller = new AbortController();
    void loadIncidents(controller.signal);
    return () => controller.abort();
  }, [filters.severity, filters.service, filters.status, filters.owner, filters.from, filters.to]);

  useEffect(() => {
    if (!selectedId) {
      setDetail(null);
      return;
    }
    const controller = new AbortController();
    setDetailLoading(true);
    apiFetch<IncidentDetail>(`/incidents/${selectedId}`, { signal: controller.signal })
      .then(setDetail)
      .catch((err) => {
        if (!(err instanceof DOMException && err.name === "AbortError")) {
          setError(err instanceof Error ? err.message : "Unable to load incident detail");
        }
      })
      .finally(() => setDetailLoading(false));
    return () => controller.abort();
  }, [selectedId]);

  const sortedIncidents = useMemo(() => {
    return [...incidents].sort((a, b) => {
      if (sort === "severity") {
        return severityRank(b.severity) - severityRank(a.severity);
      }
      if (sort === "status") {
        return statusRank(b.status) - statusRank(a.status);
      }
      return new Date(b.started_at).getTime() - new Date(a.started_at).getTime();
    });
  }, [incidents, sort]);

  const submitIncident = async (event: FormEvent) => {
    event.preventDefault();
    setActionBusy("create");
    setError("");
    try {
      const created = await apiFetch<Incident>("/incidents", {
        method: "POST",
        body: JSON.stringify({ ...form, actor: "operator-ui" })
      });
      setForm(emptyIncidentForm);
      setSelectedId(created.id);
      await loadIncidents();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to create incident");
    } finally {
      setActionBusy("");
    }
  };

  const runIncidentAction = async (action: "acknowledge" | "resolve" | "reopen" | "jira/sync") => {
    if (!selectedId) {
      return;
    }
    setActionBusy(action);
    setError("");
    try {
      await apiFetch(`/incidents/${selectedId}/${action}`, { method: "POST" });
      await loadIncidents();
      setDetail(await apiFetch<IncidentDetail>(`/incidents/${selectedId}`));
    } catch (err) {
      setError(err instanceof Error ? err.message : `Unable to run ${action}`);
    } finally {
      setActionBusy("");
    }
  };

  const metricCards = [
    ["Open", metrics?.open_incident_count ?? 0],
    ["Acknowledged", metrics?.acknowledged_incident_count ?? 0],
    ["Resolved", metrics?.resolved_incident_count ?? 0],
    ["SLA Risk", metrics?.sla_at_risk_count ?? 0]
  ];

  return (
    <section className="workspace">
      <div className="summary-strip">
        {metricCards.map(([label, value]) => (
          <div key={label} className="metric-tile">
            <span>{label}</span>
            <strong>{value}</strong>
          </div>
        ))}
      </div>

      <div className="filters-row">
        <select value={filters.severity} onChange={(e) => setFilters({ ...filters, severity: e.target.value })}>
          <option value="">Severity</option>
          <option value="critical">Critical</option>
          <option value="high">High</option>
          <option value="medium">Medium</option>
          <option value="low">Low</option>
        </select>
        <select value={filters.status} onChange={(e) => setFilters({ ...filters, status: e.target.value })}>
          <option value="">Status</option>
          <option value="open">Open</option>
          <option value="acknowledged">Acknowledged</option>
          <option value="resolved">Resolved</option>
        </select>
        <input value={filters.service} placeholder="Service" onChange={(e) => setFilters({ ...filters, service: e.target.value })} />
        <input value={filters.owner} placeholder="Owner team" onChange={(e) => setFilters({ ...filters, owner: e.target.value })} />
        <input type="date" value={filters.from} onChange={(e) => setFilters({ ...filters, from: e.target.value })} />
        <input type="date" value={filters.to} onChange={(e) => setFilters({ ...filters, to: e.target.value })} />
        <select value={sort} onChange={(e) => setSort(e.target.value as typeof sort)}>
          <option value="started">Started</option>
          <option value="severity">Severity</option>
          <option value="status">Status</option>
        </select>
        <button type="button" onClick={() => void loadIncidents()} disabled={loading}>
          Refresh
        </button>
      </div>

      {error && <div className="notice error">{error}</div>}

      <div className="split-layout">
        <section className="table-panel">
          <header className="panel-header">
            <h2>Incident Register</h2>
            <span>{loading ? "Loading" : `${sortedIncidents.length} shown`}</span>
          </header>
          {sortedIncidents.length === 0 && !loading ? (
            <EmptyState label="No incidents match filters." />
          ) : (
            <div className="incident-list">
              {sortedIncidents.map((incident) => (
                <button
                  key={incident.id}
                  className={selectedId === incident.id ? "incident-row active" : "incident-row"}
                  type="button"
                  onClick={() => setSelectedId(incident.id)}
                >
                  <span className={`severity-dot ${incident.severity}`} />
                  <span>
                    <strong>{incident.title}</strong>
                    <small>{incident.service} / {incident.environment}</small>
                  </span>
                  <span className={`pill ${incident.status}`}>{incident.status}</span>
                  <time>{formatDate(incident.started_at)}</time>
                </button>
              ))}
            </div>
          )}
        </section>

        <section className="detail-panel">
          {detailLoading ? (
            <EmptyState label="Loading incident detail." />
          ) : detail ? (
            <IncidentDetailView
              detail={detail}
              actionBusy={actionBusy}
              onAction={(action) => void runIncidentAction(action)}
            />
          ) : (
            <EmptyState label="Select an incident." />
          )}
        </section>
      </div>

      <form className="create-row" onSubmit={(event) => void submitIncident(event)}>
        <input required value={form.title} placeholder="Title" onChange={(e) => setForm({ ...form, title: e.target.value })} />
        <input value={form.summary} placeholder="Summary" onChange={(e) => setForm({ ...form, summary: e.target.value })} />
        <select value={form.severity} onChange={(e) => setForm({ ...form, severity: e.target.value })}>
          <option value="critical">Critical</option>
          <option value="high">High</option>
          <option value="medium">Medium</option>
          <option value="low">Low</option>
        </select>
        <input required value={form.service} placeholder="Service" onChange={(e) => setForm({ ...form, service: e.target.value })} />
        <input required value={form.environment} placeholder="Environment" onChange={(e) => setForm({ ...form, environment: e.target.value })} />
        <input required value={form.owner_team} placeholder="Owner team" onChange={(e) => setForm({ ...form, owner_team: e.target.value })} />
        <button type="submit" disabled={actionBusy === "create"}>Create</button>
      </form>
    </section>
  );
}

function IncidentDetailView({
  detail,
  actionBusy,
  onAction
}: {
  detail: IncidentDetail;
  actionBusy: string;
  onAction: (action: "acknowledge" | "resolve" | "reopen" | "jira/sync") => void;
}) {
  const incident = detail.incident;
  return (
    <>
      <header className="detail-header">
        <div>
          <span className={`pill ${incident.status}`}>{incident.status}</span>
          <h2>{incident.title}</h2>
          <p>{incident.summary || "No summary captured."}</p>
        </div>
        <div className="action-group">
          <button type="button" onClick={() => onAction("acknowledge")} disabled={actionBusy !== "" || incident.status !== "open"}>
            Ack
          </button>
          <button type="button" onClick={() => onAction("resolve")} disabled={actionBusy !== "" || incident.status === "resolved"}>
            Resolve
          </button>
          <button type="button" onClick={() => onAction("reopen")} disabled={actionBusy !== "" || incident.status !== "resolved"}>
            Reopen
          </button>
          <button type="button" onClick={() => onAction("jira/sync")} disabled={actionBusy !== ""}>
            Sync JIRA
          </button>
        </div>
      </header>

      <div className="detail-grid">
        <DataPoint label="Severity" value={incident.severity} />
        <DataPoint label="Service" value={incident.service} />
        <DataPoint label="Environment" value={incident.environment} />
        <DataPoint label="Owner" value={incident.owner_team} />
        <DataPoint label="Started" value={formatDate(incident.started_at)} />
        <DataPoint label="JIRA" value={detail.jira?.jira_issue_key ?? "-"} />
      </div>

      <div className="detail-columns">
        <section>
          <h3>Timeline</h3>
          {detail.events.length === 0 ? (
            <EmptyState label="No timeline events." compact />
          ) : (
            <ol className="timeline">
              {detail.events.map((event) => (
                <li key={event.id}>
                  <strong>{event.type}</strong>
                  <span>{event.actor} / {formatDate(event.created_at)}</span>
                </li>
              ))}
            </ol>
          )}
        </section>
        <section>
          <h3>Linked Alerts</h3>
          {detail.alerts.length === 0 ? (
            <EmptyState label="No linked alerts." compact />
          ) : (
            <div className="alert-list">
              {detail.alerts.map((alert) => (
                <div key={alert.id} className="alert-row">
                  <strong>{alert.source}</strong>
                  <span>{alert.source_event_id}</span>
                  <small>{formatDate(alert.observed_at)}</small>
                </div>
              ))}
            </div>
          )}
        </section>
      </div>
    </>
  );
}

function ReportsWorkspace() {
  const [filters, setFilters] = useState<ReportFilters>({ from: "", to: "", service: "", environment: "", team: "" });
  const [live, setLive] = useState<LiveMetrics | null>(null);
  const [sre, setSRE] = useState<SREReport | null>(null);
  const [dora, setDORA] = useState<DORAReport | null>(null);
  const [executive, setExecutive] = useState<ExecutiveReport | null>(null);
  const [lastRefresh, setLastRefresh] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const query = buildQuery({
    from: filters.from,
    to: filters.to,
    service: filters.service,
    environment: filters.environment,
    team: filters.team
  });

	const loadReports = async (signal?: AbortSignal) => {
    setLoading(true);
    setError("");
    try {
      const [liveOut, sreOut, doraOut, executiveOut] = await Promise.all([
        apiFetch<LiveMetrics>(`/metrics/live${query}`, { signal }),
        apiFetch<SREReport>(`/reports/sre${query}`, { signal }),
        apiFetch<DORAReport>(`/reports/dora${query}`, { signal }),
        apiFetch<ExecutiveReport>(`/reports/executive${query}`, { signal })
      ]);
      setLive(liveOut);
      setSRE(sreOut);
      setDORA(doraOut);
      setExecutive(executiveOut);
      setLastRefresh(new Date().toLocaleTimeString());
    } catch (err) {
      if (!(err instanceof DOMException && err.name === "AbortError")) {
        setError(err instanceof Error ? err.message : "Unable to load reports");
      }
    } finally {
      setLoading(false);
		}
	};

	const downloadCSV = async () => {
		setError("");
		try {
			const response = await fetch(`${API_BASE_URL}/exports/incidents.csv${query}`, {
				headers: apiHeaders({ "Content-Type": "text/csv" })
			});
			if (!response.ok) {
				throw new Error(`CSV export failed with HTTP ${response.status}`);
			}
			const blob = await response.blob();
			const url = window.URL.createObjectURL(blob);
			const anchor = document.createElement("a");
			anchor.href = url;
			anchor.download = "incidents.csv";
			anchor.click();
			window.URL.revokeObjectURL(url);
		} catch (err) {
			setError(err instanceof Error ? err.message : "Unable to export CSV");
		}
	};

  useEffect(() => {
    const controller = new AbortController();
    void loadReports(controller.signal);
    const timer = window.setInterval(() => void loadReports(), 30000);
    return () => {
      controller.abort();
      window.clearInterval(timer);
    };
  }, [query]);

	return (
    <section className="workspace">
      <div className="filters-row">
        <input type="date" value={filters.from} onChange={(e) => setFilters({ ...filters, from: e.target.value })} />
        <input type="date" value={filters.to} onChange={(e) => setFilters({ ...filters, to: e.target.value })} />
        <input value={filters.service} placeholder="Service" onChange={(e) => setFilters({ ...filters, service: e.target.value })} />
        <input value={filters.environment} placeholder="Environment" onChange={(e) => setFilters({ ...filters, environment: e.target.value })} />
        <input value={filters.team} placeholder="Team" onChange={(e) => setFilters({ ...filters, team: e.target.value })} />
        <button type="button" onClick={() => void loadReports()} disabled={loading}>Refresh</button>
				<button type="button" onClick={() => void downloadCSV()}>CSV</button>
      </div>

      {error && <div className="notice error">{error}</div>}
      <div className="panel-header inline">
        <h2>Reporting</h2>
        <span>{loading ? "Loading" : `Updated ${lastRefresh || "-"}`}</span>
      </div>

      <div className="summary-strip">
        <DataMetric label="Incidents" value={live?.incident_count ?? 0} />
        <DataMetric label="MTTD" value={formatDuration(sre?.mttd_seconds)} />
        <DataMetric label="MTTA" value={formatDuration(sre?.mtta_seconds)} />
        <DataMetric label="MTTR" value={formatDuration(sre?.mttr_seconds)} />
        <DataMetric label="SLA Breach" value={live?.sla_breach_count ?? 0} />
      </div>

      <div className="report-grid">
        <section className="report-band">
          <h3>SRE</h3>
          <KeyValueRows
            rows={[
              ["Alerts", live?.alert_count ?? 0],
              ["Alert conversion", percent(live?.alert_to_incident_conversion ?? 0)],
              ["Duplicate suppression", live?.duplicate_suppression_count ?? 0],
              ["Reopen rate", percent(live?.reopen_rate ?? 0)]
            ]}
          />
        </section>
        <section className="report-band">
          <h3>DORA</h3>
          <KeyValueRows
            rows={[
              ["Deployments", dora?.deployment_count ?? 0],
              ["Linked incidents", dora?.deployment_linked_incidents ?? 0],
              ["Change failure", percent(dora?.change_failure_rate ?? 0)],
              ["Restore time", formatDuration(dora?.time_to_restore_service_seconds)]
            ]}
          />
        </section>
        <section className="report-band">
          <h3>Executive</h3>
          <KeyValueRows
            rows={[
              ["Downtime", formatDuration(executive?.downtime_seconds)],
              ["SLA risk", executive?.sla_at_risk_count ?? 0],
              ["SLA breach", executive?.sla_breach_count ?? 0],
              ["Services", Object.keys(executive?.service_reliability ?? {}).length]
            ]}
          />
        </section>
      </div>

      <section className="table-panel">
        <header className="panel-header">
          <h2>Severity Trends</h2>
          <span>{executive?.generated_at ? formatDate(executive.generated_at) : "-"}</span>
        </header>
        <BarList values={executive?.severity_trends ?? {}} />
      </section>
    </section>
  );
}

function IntegrationsWorkspace() {
  const [statuses, setStatuses] = useState<IntegrationStatus[]>([]);
  const [events, setEvents] = useState<IntegrationEvent[]>([]);
  const [routes, setRoutes] = useState<TeamsRoute[]>([]);
  const [routeForm, setRouteForm] = useState<TeamsRouteForm>(emptyTeamsRouteForm);
  const [teamOptions, setTeamOptions] = useState<DirectoryEntry[]>([]);
  const [channelOptions, setChannelOptions] = useState<DirectoryEntry[]>([]);
  const [userResults, setUserResults] = useState<DirectoryEntry[]>([]);
  const [tagResults, setTagResults] = useState<DirectoryEntry[]>([]);
  const [userQuery, setUserQuery] = useState("");
  const [connectForm, setConnectForm] = useState({
    access_token: "",
    refresh_token: "",
    expires_at: "",
    scopes: "offline_access ChannelMessage.Send Team.ReadBasic.All ChannelMember.Read.All User.Read"
  });
  const [result, setResult] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");

  const teamsStatus = statuses.find((status) => status.name === "teams");

  const load = async () => {
    setLoading(true);
    setError("");
    try {
      const [statusOut, eventsOut, routesOut] = await Promise.all([
        apiFetch<IntegrationStatus[]>("/integrations"),
        apiFetch<IntegrationEvent[]>("/integration-events?limit=25"),
        apiFetch<TeamsRoute[]>("/integrations/teams/routes")
      ]);
      setStatuses(statusOut);
      setEvents(eventsOut);
      setRoutes(routesOut);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to load integrations");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const testJIRA = async () => {
    setResult("");
    setError("");
    try {
      setBusy("jira-test");
      const out = await apiFetch<{ success: boolean; message: string }>("/integrations/jira/test", { method: "POST" });
      setResult(out.message);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to test JIRA");
      await load();
    } finally {
      setBusy("");
    }
  };

  const testTeams = async () => {
    setBusy("teams-test");
    setResult("");
    setError("");
    try {
      const out = await apiFetch<{ success: boolean; message: string }>("/integrations/teams/test", { method: "POST" });
      setResult(out.message);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to test Teams");
      await load();
    } finally {
      setBusy("");
    }
  };

  const connectTeams = async (event: FormEvent) => {
    event.preventDefault();
    setBusy("teams-connect");
    setResult("");
    setError("");
    try {
      await apiFetch("/integrations/teams/connect", {
        method: "POST",
        body: JSON.stringify({
          access_token: connectForm.access_token,
          refresh_token: connectForm.refresh_token,
          expires_at: connectForm.expires_at ? new Date(connectForm.expires_at).toISOString() : undefined,
          scopes: connectForm.scopes
        })
      });
      setConnectForm({ ...connectForm, access_token: "", refresh_token: "" });
      setResult("Teams connection saved.");
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to connect Teams");
    } finally {
      setBusy("");
    }
  };

  const loadTeams = async () => {
    setBusy("load-teams");
    setError("");
    try {
      setTeamOptions(await apiFetch<DirectoryEntry[]>("/integrations/teams/lookup/teams"));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to load teams");
    } finally {
      setBusy("");
    }
  };

  const loadChannels = async (teamId: string) => {
    if (!teamId) {
      setChannelOptions([]);
      return;
    }
    setBusy("load-channels");
    setError("");
    try {
      setChannelOptions(await apiFetch<DirectoryEntry[]>(`/integrations/teams/lookup/teams/${teamId}`));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to load channels");
    } finally {
      setBusy("");
    }
  };

  const searchUsers = async () => {
    setBusy("search-users");
    setError("");
    try {
      setUserResults(await apiFetch<DirectoryEntry[]>(`/integrations/teams/lookup/users${buildQuery({ q: userQuery })}`));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to search users");
    } finally {
      setBusy("");
    }
  };

  const loadTags = async (teamId: string) => {
    if (!teamId) {
      setTagResults([]);
      return;
    }
    setBusy("load-tags");
    setError("");
    try {
      setTagResults(await apiFetch<DirectoryEntry[]>(`/integrations/teams/lookup/tags${buildQuery({ team_id: teamId })}`));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to load tags");
    } finally {
      setBusy("");
    }
  };

  const addRecipient = (recipient: TeamsRouteRecipient) => {
    const exists = routeForm.recipients.some((current) => current.type === recipient.type && current.teams_object_id === recipient.teams_object_id);
    if (exists) {
      return;
    }
    setRouteForm({ ...routeForm, recipients: [...routeForm.recipients, recipient] });
  };

  const removeRecipient = (recipient: TeamsRouteRecipient) => {
    setRouteForm({
      ...routeForm,
      recipients: routeForm.recipients.filter((current) => !(current.type === recipient.type && current.teams_object_id === recipient.teams_object_id))
    });
  };

  const resetRouteForm = () => {
    setRouteForm(emptyTeamsRouteForm);
    setChannelOptions([]);
    setUserResults([]);
    setTagResults([]);
  };

  const saveRoute = async (event: FormEvent) => {
    event.preventDefault();
    setBusy("save-route");
    setError("");
    setResult("");
    const payload = {
      name: routeForm.name,
      enabled: routeForm.enabled,
      team_id: routeForm.team_id,
      team_name: routeForm.team_name,
      channel_id: routeForm.channel_id,
      channel_name: routeForm.channel_name,
      owner_team: routeForm.owner_team,
      service: routeForm.service,
      environment: routeForm.environment,
      severity_min: routeForm.severity_min,
      recipients: routeForm.recipients.map((recipient) => ({
        type: recipient.type,
        teams_object_id: recipient.teams_object_id,
        display_name: recipient.display_name,
        upn: recipient.upn
      }))
    };
    try {
      if (routeForm.id) {
        await apiFetch(`/integrations/teams/routes/${routeForm.id}`, {
          method: "PATCH",
          body: JSON.stringify(payload)
        });
        setResult("Teams route updated.");
      } else {
        await apiFetch("/integrations/teams/routes", {
          method: "POST",
          body: JSON.stringify(payload)
        });
        setResult("Teams route created.");
      }
      resetRouteForm();
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to save route");
    } finally {
      setBusy("");
    }
  };

  const deleteRoute = async (id: string) => {
    setBusy(`delete-${id}`);
    setError("");
    setResult("");
    try {
      await apiFetch(`/integrations/teams/routes/${id}`, { method: "DELETE" });
      if (routeForm.id === id) {
        resetRouteForm();
      }
      setResult("Teams route deleted.");
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to delete route");
    } finally {
      setBusy("");
    }
  };

  const testRoute = async (id: string) => {
    setBusy(`route-test-${id}`);
    setError("");
    setResult("");
    try {
      await apiFetch(`/integrations/teams/routes/${id}/test`, { method: "POST" });
      setResult("Teams route test sent.");
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to test Teams route");
      await load();
    } finally {
      setBusy("");
    }
  };

  const editRoute = async (route: TeamsRoute) => {
    setRouteForm(routeToForm(route));
    if (route.team_id) {
      await loadChannels(route.team_id);
      await loadTags(route.team_id);
    }
  };

  return (
    <section className="workspace">
      {error && <div className="notice error">{error}</div>}
      {result && <div className="notice success">{result}</div>}
      <div className="report-grid">
        {statuses.length === 0 && !loading ? (
          <EmptyState label="No integrations returned." />
        ) : statuses.map((status) => (
          <section key={status.name} className="report-band">
            <h2>{status.name.toUpperCase()}</h2>
            <KeyValueRows
              rows={[
                ["Enabled", status.enabled ? "yes" : "no"],
                ["Configured", status.configured ? "yes" : "no"],
                ["Base URL", status.base_url ?? "-"],
                ["Project", status.project_key ?? "-"],
                ["Connected", status.connected ? "yes" : "no"],
                ["Sender", status.sender_display_name ?? "-"],
                ["Sender UPN", status.sender_upn ?? "-"]
              ]}
            />
            {status.name === "jira" && <button type="button" onClick={() => void testJIRA()} disabled={busy !== ""}>Test</button>}
            {status.name === "teams" && <button type="button" onClick={() => void testTeams()} disabled={busy !== ""}>Test</button>}
          </section>
        ))}
      </div>

      <section className="table-panel">
        <header className="panel-header">
          <h2>Teams Connection</h2>
          <div className="action-group">
            <button type="button" onClick={() => void loadTeams()} disabled={busy !== ""}>Load Teams</button>
            <button type="button" onClick={() => void testTeams()} disabled={busy !== ""}>Verify</button>
          </div>
        </header>
        <form className="create-row integration-form" onSubmit={(event) => void connectTeams(event)}>
          <input
            required
            value={connectForm.access_token}
            placeholder="Access token"
            onChange={(e) => setConnectForm({ ...connectForm, access_token: e.target.value })}
          />
          <input
            value={connectForm.refresh_token}
            placeholder="Refresh token"
            onChange={(e) => setConnectForm({ ...connectForm, refresh_token: e.target.value })}
          />
          <input
            value={connectForm.scopes}
            placeholder="Scopes"
            onChange={(e) => setConnectForm({ ...connectForm, scopes: e.target.value })}
          />
          <input
            type="datetime-local"
            value={connectForm.expires_at}
            onChange={(e) => setConnectForm({ ...connectForm, expires_at: e.target.value })}
          />
          <button type="submit" disabled={busy !== ""}>Save Connection</button>
        </form>
        {teamsStatus && (
          <small>
            Tenant: {teamsStatus.tenant_id ?? "-"} / Connected: {teamsStatus.connected ? "yes" : "no"}
          </small>
        )}
      </section>

      <div className="split-layout integrations-layout">
        <section className="table-panel">
          <header className="panel-header">
            <h2>Teams Routes</h2>
            <span>{routes.length} configured</span>
          </header>
          {routes.length === 0 && !loading ? (
            <EmptyState label="No Teams routes configured." />
          ) : (
            <div className="event-table">
              {routes.map((route) => (
                <div key={route.id} className="event-row">
                  <span className={`pill ${route.enabled ? "open" : "resolved"}`}>{route.enabled ? "enabled" : "disabled"}</span>
                  <strong>{route.name}</strong>
                  <span>{route.team_name || route.team_id}</span>
                  <span>{route.channel_name || route.channel_id}</span>
                  <span>{route.recipients.length} recipients</span>
                  <div className="action-group">
                    <button type="button" onClick={() => void editRoute(route)} disabled={busy !== ""}>Edit</button>
                    <button type="button" onClick={() => void testRoute(route.id)} disabled={busy !== ""}>Test</button>
                    <button type="button" onClick={() => void deleteRoute(route.id)} disabled={busy !== ""}>Delete</button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>

        <section className="detail-panel">
          <header className="panel-header">
            <h2>{routeForm.id ? "Edit Teams Route" : "New Teams Route"}</h2>
            {routeForm.id && <button type="button" onClick={resetRouteForm}>Clear</button>}
          </header>
          <form className="integration-editor" onSubmit={(event) => void saveRoute(event)}>
            <input required value={routeForm.name} placeholder="Route name" onChange={(e) => setRouteForm({ ...routeForm, name: e.target.value })} />
            <label className="checkbox-row">
              <input type="checkbox" checked={routeForm.enabled} onChange={(e) => setRouteForm({ ...routeForm, enabled: e.target.checked })} />
              Enabled
            </label>
            <div className="filters-row">
              <select
                value={routeForm.team_id}
                onChange={(e) => {
                  const selected = teamOptions.find((team) => team.id === e.target.value);
                  setRouteForm({
                    ...routeForm,
                    team_id: e.target.value,
                    team_name: selected?.display_name ?? "",
                    channel_id: "",
                    channel_name: ""
                  });
                  void loadChannels(e.target.value);
                  void loadTags(e.target.value);
                }}
              >
                <option value="">Team</option>
                {teamOptions.map((team) => (
                  <option key={team.id} value={team.id}>{team.display_name}</option>
                ))}
              </select>
              <select
                value={routeForm.channel_id}
                onChange={(e) => {
                  const selected = channelOptions.find((channel) => channel.id === e.target.value);
                  setRouteForm({
                    ...routeForm,
                    channel_id: e.target.value,
                    channel_name: selected?.display_name ?? ""
                  });
                }}
              >
                <option value="">Channel</option>
                {channelOptions.map((channel) => (
                  <option key={channel.id} value={channel.id}>{channel.display_name}</option>
                ))}
              </select>
              <input value={routeForm.owner_team} placeholder="Owner team filter" onChange={(e) => setRouteForm({ ...routeForm, owner_team: e.target.value })} />
              <input value={routeForm.service} placeholder="Service filter" onChange={(e) => setRouteForm({ ...routeForm, service: e.target.value })} />
              <input value={routeForm.environment} placeholder="Environment filter" onChange={(e) => setRouteForm({ ...routeForm, environment: e.target.value })} />
              <select value={routeForm.severity_min} onChange={(e) => setRouteForm({ ...routeForm, severity_min: e.target.value })}>
                <option value="">Min severity</option>
                <option value="critical">Critical</option>
                <option value="high">High</option>
                <option value="medium">Medium</option>
                <option value="low">Low</option>
              </select>
            </div>

            <div className="detail-columns">
              <section>
                <h3>Recipients</h3>
                {routeForm.recipients.length === 0 ? (
                  <EmptyState label="No recipients selected." compact />
                ) : (
                  <div className="event-table">
                    {routeForm.recipients.map((recipient) => (
                      <div key={`${recipient.type}-${recipient.teams_object_id}`} className="event-row">
                        <span className={`pill ${recipient.type === "user" ? "acknowledged" : "open"}`}>{recipient.type}</span>
                        <strong>{recipient.display_name}</strong>
                        <span>{recipient.upn ?? recipient.teams_object_id}</span>
                        <button type="button" onClick={() => removeRecipient(recipient)}>Remove</button>
                      </div>
                    ))}
                  </div>
                )}
              </section>

              <section>
                <h3>User Search</h3>
                <div className="filters-row">
                  <input value={userQuery} placeholder="Search users" onChange={(e) => setUserQuery(e.target.value)} />
                  <button type="button" onClick={() => void searchUsers()} disabled={busy !== ""}>Search</button>
                  <button type="button" onClick={() => void loadTags(routeForm.team_id)} disabled={busy !== "" || !routeForm.team_id}>Load Tags</button>
                </div>
                <div className="event-table">
                  {userResults.map((user) => (
                    <div key={user.id} className="event-row">
                      <strong>{user.display_name}</strong>
                      <span>{user.upn ?? "-"}</span>
                      <button
                        type="button"
                        onClick={() => addRecipient({ type: "user", teams_object_id: user.id, display_name: user.display_name, upn: user.upn })}
                      >
                        Add
                      </button>
                    </div>
                  ))}
                  {tagResults.map((tag) => (
                    <div key={tag.id} className="event-row">
                      <strong>{tag.display_name}</strong>
                      <span>{tag.description ?? "Teams tag"}</span>
                      <button
                        type="button"
                        onClick={() => addRecipient({ type: "tag", teams_object_id: tag.id, display_name: tag.display_name })}
                      >
                        Add Tag
                      </button>
                    </div>
                  ))}
                </div>
              </section>
            </div>

            <button type="submit" disabled={busy !== "" || routeForm.recipients.length === 0}>
              {routeForm.id ? "Update Route" : "Create Route"}
            </button>
          </form>
        </section>
      </div>

      <section className="table-panel">
        <header className="panel-header">
          <h2>Integration Events</h2>
          <button type="button" onClick={() => void load()} disabled={loading}>Refresh</button>
        </header>
        {events.length === 0 && !loading ? (
          <EmptyState label="No integration events." />
        ) : (
          <div className="event-table">
            {events.map((event) => (
              <div key={event.id} className="event-row">
                <span className={`pill ${event.status}`}>{event.status}</span>
                <strong>{event.type}</strong>
                <span>{event.integration_name}</span>
                <span>{event.attempts} attempts</span>
                <time>{formatDate(event.updated_at)}</time>
              </div>
            ))}
          </div>
        )}
      </section>
    </section>
  );
}

function TemplatesWorkspace() {
  const [template, setTemplate] = useState(defaultTemplate);
  const [context, setContext] = useState(defaultTemplateContext);
  const [validation, setValidation] = useState<Record<string, unknown> | null>(null);
  const [preview, setPreview] = useState<Record<string, unknown> | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");

  const validate = async () => {
    setBusy("validate");
    setError("");
    setPreview(null);
    try {
      const out = await apiFetch<Record<string, unknown>>("/ticket-templates/validate", {
        method: "POST",
        body: JSON.stringify({ template_yaml: template })
      });
      setValidation(out);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to validate template");
    } finally {
      setBusy("");
    }
  };

  const runPreview = async () => {
    setBusy("preview");
    setError("");
    try {
      const parsedContext = JSON.parse(context) as Record<string, unknown>;
      const out = await apiFetch<{ rendered: Record<string, unknown> }>("/ticket-templates/preview", {
        method: "POST",
        body: JSON.stringify({ template_yaml: template, context: parsedContext })
      });
      setPreview(out.rendered);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Unable to preview template");
    } finally {
      setBusy("");
    }
  };

  return (
    <section className="workspace template-layout">
      <div className="editor-grid">
        <section>
          <header className="panel-header">
            <h2>Template YAML</h2>
            <div className="action-group">
              <button type="button" onClick={() => void validate()} disabled={busy !== ""}>Validate</button>
              <button type="button" onClick={() => void runPreview()} disabled={busy !== ""}>Preview</button>
            </div>
          </header>
          <textarea className="code-editor" value={template} onChange={(e) => setTemplate(e.target.value)} spellCheck={false} />
        </section>
        <section>
          <header className="panel-header">
            <h2>Preview Context</h2>
          </header>
          <textarea className="code-editor" value={context} onChange={(e) => setContext(e.target.value)} spellCheck={false} />
        </section>
      </div>

      {error && <div className="notice error">{error}</div>}
      <div className="output-grid">
        <OutputBlock title="Validation" value={validation} />
        <OutputBlock title="Rendered Ticket" value={preview} />
      </div>
    </section>
  );
}

function DataPoint({ label, value }: { label: string; value: string }) {
  return (
    <div className="data-point">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function DataMetric({ label, value }: { label: string; value: string | number }) {
  return (
    <div className="metric-tile">
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function KeyValueRows({ rows }: { rows: Array<[string, string | number]> }) {
  return (
    <dl className="key-values">
      {rows.map(([key, value]) => (
        <div key={key}>
          <dt>{key}</dt>
          <dd>{value}</dd>
        </div>
      ))}
    </dl>
  );
}

function BarList({ values }: { values: Record<string, number> }) {
  const entries = Object.entries(values);
  const max = Math.max(1, ...entries.map(([, value]) => value));
  if (entries.length === 0) {
    return <EmptyState label="No trend data." compact />;
  }
  return (
    <div className="bar-list">
      {entries.map(([label, value]) => (
        <div key={label} className="bar-row">
          <span>{label}</span>
          <div className="bar-track"><span style={{ width: `${(value / max) * 100}%` }} /></div>
          <strong>{value}</strong>
        </div>
      ))}
    </div>
  );
}

function OutputBlock({ title, value }: { title: string; value: Record<string, unknown> | null }) {
  return (
    <section className="output-block">
      <h2>{title}</h2>
      {value ? <pre>{JSON.stringify(value, null, 2)}</pre> : <EmptyState label="No output." compact />}
    </section>
  );
}

function EmptyState({ label, compact = false }: { label: string; compact?: boolean }) {
  return <div className={compact ? "empty compact" : "empty"}>{label}</div>;
}
