export interface MeResponse {
  id: number;
  username: string;
  totp_enabled: boolean;
}

export interface SetupState {
  setup_required: boolean;
}

export interface HealthStatus {
  status: string;
  version: string;
}

export interface Group {
  id: number;
  name: string;
  enabled: boolean;
}

export interface Client {
  id: number;
  name: string;
  matcher: string;
  group_id: number;
}

export interface List {
  id: number;
  url: string;
  kind: "block" | "allow";
  enabled: boolean;
  last_refreshed: number;
  entry_count: number;
}

export interface Rule {
  id: number;
  group_id: number;
  action: "allow" | "block";
  pattern: string;
  is_regex: boolean;
}

export interface LocalRecord {
  id: number;
  name: string;
  type: "A" | "AAAA" | "CNAME" | "TXT";
  value: string;
  ttl: number;
}

export interface QueryEntry {
  id: number;
  at: number;
  client_ip: string;
  client_id: number;
  q_name: string;
  q_type: string;
  decision: string;
  rule_id: number;
  list_id: number;
  upstream: string;
  r_code: string;
  duration_ms: number;
}

export interface StatsOverview {
  total: number;
  blocked: number;
  cached: number;
  forwarded: number;
  clients: number;
}

export interface TimelineBucket {
  bucket: number;
  decisions: Record<string, number>;
}

export interface TopEntry {
  key: string;
  count: number;
}

export interface ApiToken {
  id: number;
  name: string;
  scope: "read" | "write";
  created_at: number;
  last_used: number;
}

export type Settings = Record<string, string>;
