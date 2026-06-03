import { describe, expect, it } from "vitest";

import { emptyTeamsRouteForm, routeToForm } from "./App";

describe("routeToForm", () => {
  it("maps route payload into editable form state", () => {
    const form = routeToForm({
      id: "route-1",
      name: "Primary on-call",
      enabled: true,
      team_id: "team-1",
      team_name: "Operations",
      channel_id: "channel-1",
      channel_name: "Incidents",
      owner_team: "sre",
      service: "checkout",
      environment: "prod",
      severity_min: "high",
      recipients: [
        {
          type: "user",
          teams_object_id: "user-1",
          display_name: "Alice Example",
          upn: "alice@example.com"
        }
      ]
    });

    expect(form).toEqual({
      id: "route-1",
      name: "Primary on-call",
      enabled: true,
      team_id: "team-1",
      team_name: "Operations",
      channel_id: "channel-1",
      channel_name: "Incidents",
      owner_team: "sre",
      service: "checkout",
      environment: "prod",
      severity_min: "high",
      recipients: [
        {
          type: "user",
          teams_object_id: "user-1",
          display_name: "Alice Example",
          upn: "alice@example.com"
        }
      ]
    });
  });

  it("provides an empty editable route baseline", () => {
    expect(emptyTeamsRouteForm.enabled).toBe(true);
    expect(emptyTeamsRouteForm.recipients).toEqual([]);
  });
});
