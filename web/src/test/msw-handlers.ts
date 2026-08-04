import { http, HttpResponse } from "msw";
import type { MeResponse, SetupState, Settings } from "../api/types";

export const handlers = [
  http.get("/api/v1/auth/me", () => {
    const me: MeResponse = { id: 1, username: "admin", totp_enabled: false };
    return HttpResponse.json(me);
  }),

  http.get("/api/v1/setup", () => {
    const setupState: SetupState = { setup_required: false };
    return HttpResponse.json(setupState);
  }),

  http.get("/api/v1/settings", () => {
    const settings: Settings = {};
    return HttpResponse.json(settings);
  }),
];
