import { apiRequest } from "@/shared/api/client";
import {
  createValidatedDecoder,
  hasShape,
  isArrayOf,
  isBoolean,
  isNumber,
  isOneOf,
  isString,
  type ApiDecoder,
  type ValueValidator,
} from "@/shared/api/decoder";

/** 注册机 sidecar 的任务状态；queued/running 为进行中，其余为终态。 */
export type RegistrarJobStatus = "queued" | "running" | "done" | "failed" | "canceled";

export type RegistrarStatus = {
  available: boolean;
  enabled: boolean;
  maxCount: number;
  domains: string[];
};

/** 单条注册结果；接口不下发密码和 SSO，前端也不应请求。 */
export type RegistrarJobItem = {
  email: string;
  status: string;
  reason: string;
};

export type RegistrarJob = {
  jobId: string;
  status: RegistrarJobStatus;
  count: number;
  created: number;
  imported: number;
  failed: number;
  startedAt: number;
  finishedAt: number;
  lastError: string;
  items: RegistrarJobItem[];
};

export const registrarStatusQueryKey = ["accounts-registrar", "status"] as const;

export function registrarJobQueryKey(jobId: string | null): readonly ["accounts-registrar", "job", string | null] {
  return ["accounts-registrar", "job", jobId] as const;
}

export function isRegistrarJobActive(status: RegistrarJobStatus): boolean {
  return status === "queued" || status === "running";
}

// 后端直接透传 sidecar 的 Go 结构体，nil 切片会序列化成 null，这里要把 null 归一成空数组。
function isNullableArrayOf(validator: ValueValidator): ValueValidator {
  const isArray = isArrayOf(validator);
  return (value) => value === null || isArray(value);
}

const registrarJobItemValidator = hasShape({
  email: isString,
  status: isString,
  reason: isString,
});

type RegistrarStatusWire = Omit<RegistrarStatus, "domains"> & { domains: string[] | null };
type RegistrarJobWire = Omit<RegistrarJob, "items"> & { items: RegistrarJobItem[] | null };

const decodeRegistrarStatusWire = createValidatedDecoder<RegistrarStatusWire>("registrar status", hasShape({
  available: isBoolean,
  enabled: isBoolean,
  maxCount: isNumber,
  domains: isNullableArrayOf(isString),
}));

const decodeRegistrarJobWire = createValidatedDecoder<RegistrarJobWire>("registrar job", hasShape({
  jobId: isString,
  status: isOneOf("queued", "running", "done", "failed", "canceled"),
  count: isNumber,
  created: isNumber,
  imported: isNumber,
  failed: isNumber,
  startedAt: isNumber,
  finishedAt: isNumber,
  lastError: isString,
  items: isNullableArrayOf(registrarJobItemValidator),
}));

const decodeRegistrarStatus: ApiDecoder<RegistrarStatus> = (value) => {
  const status = decodeRegistrarStatusWire(value);
  return { ...status, domains: status.domains ?? [] };
};

const decodeRegistrarJob: ApiDecoder<RegistrarJob> = (value) => {
  const job = decodeRegistrarJobWire(value);
  return { ...job, items: job.items ?? [] };
};

export function getRegistrarStatus(): Promise<RegistrarStatus> {
  return apiRequest("/api/admin/v1/accounts/registrar/status", {}, decodeRegistrarStatus);
}

export function createRegistrarJob(input: { count: number; domains?: string[] }): Promise<RegistrarJob> {
  const body: { count: number; domains?: string[] } = { count: input.count };
  if (input.domains && input.domains.length > 0) body.domains = input.domains;
  return apiRequest("/api/admin/v1/accounts/registrar/jobs", { method: "POST", body }, decodeRegistrarJob);
}

export function getRegistrarJob(jobId: string): Promise<RegistrarJob> {
  return apiRequest(`/api/admin/v1/accounts/registrar/jobs/${encodeURIComponent(jobId)}`, {}, decodeRegistrarJob);
}

export function cancelRegistrarJob(jobId: string): Promise<RegistrarJob> {
  return apiRequest(`/api/admin/v1/accounts/registrar/jobs/${encodeURIComponent(jobId)}/cancel`, { method: "POST" }, decodeRegistrarJob);
}
