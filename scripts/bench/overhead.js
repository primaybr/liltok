// k6 gateway-overhead benchmark. Works against any OpenAI-compatible gateway.
//
//   k6 run -e GATEWAY=liltok -e BASE_URL=http://127.0.0.1:8181 -e API_KEY=bench-key scripts/bench/overhead.js
//   k6 run -e GATEWAY=direct -e BASE_URL=http://127.0.0.1:9999 scripts/bench/overhead.js   (baseline)
//
// Scenarios run one after another (no overlap):
//   miss       unique prompt per request -> always reaches the mock upstream (pure overhead)
//   cache_hit  fixed 200-prompt corpus, warmed in setup() -> measures the cache path
//
// Added latency = gateway miss latency minus the GATEWAY=direct miss latency (same RATE).
// Results (P50/P95/P99, throughput, error rate) are written to RESULT_FILE as JSON.
//
// Environment:
//   GATEWAY      label stored in the result (default gateway)
//   BASE_URL     gateway address (default http://127.0.0.1:8181)
//   API_KEY      bearer token, optional
//   MODEL        default gpt-4o-mini
//   MISS_RATE    req/s for the miss scenario (default 200)
//   HIT_RATE     req/s for the cache-hit scenario (default 2000)
//   DURATION     seconds-style duration per scenario (default 30s)
//   VUS          preallocated VUs (default 100)
//   RESULT_FILE  output JSON path (default scripts/bench/results/<GATEWAY>.json)
//   RUN_ID       uniquifier for miss prompts (default Date.now())

import http from 'k6/http';
import { check, fail, sleep } from 'k6';

const GATEWAY = __ENV.GATEWAY || 'gateway';
const BASE_URL = __ENV.BASE_URL || 'http://127.0.0.1:8181';
const MODEL = __ENV.MODEL || 'gpt-4o-mini';
const MISS_RATE = parseInt(__ENV.MISS_RATE || '200', 10);
const HIT_RATE = parseInt(__ENV.HIT_RATE || '2000', 10);
const DURATION = __ENV.DURATION || '30s';
const VUS = parseInt(__ENV.VUS || '100', 10);
const RUN_ID = __ENV.RUN_ID || String(Date.now());
const RESULT_FILE = __ENV.RESULT_FILE || `scripts/bench/results/${GATEWAY}.json`;
const DURATION_SECONDS = parseInt(DURATION, 10);

const URL = `${BASE_URL}/v1/chat/completions`;
const HEADERS = { 'Content-Type': 'application/json' };
if (__ENV.API_KEY) {
  HEADERS.Authorization = `Bearer ${__ENV.API_KEY}`;
}

// Fixed corpus: 200 prompts generated from a formula, identical on every run.
const TOPICS = ['caching', 'routing', 'latency', 'tokens', 'retries', 'streaming', 'budgets',
  'logging', 'proxies', 'sharding', 'queues', 'indexes', 'locks', 'timeouts', 'batching',
  'backoff', 'quotas', 'metrics', 'tracing', 'failover'];
const CORPUS = [];
for (let i = 0; i < 200; i++) {
  CORPUS.push(`Corpus prompt ${i}: explain ${TOPICS[i % TOPICS.length]} in exactly ${(i % 7) + 3} sentences.`);
}

function payload(prompt) {
  return JSON.stringify({ model: MODEL, temperature: 0, messages: [{ role: 'user', content: prompt }] });
}

export const options = {
  scenarios: {
    miss: {
      executor: 'constant-arrival-rate',
      exec: 'miss',
      rate: MISS_RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: VUS,
      maxVUs: VUS * 4,
      startTime: '0s',
    },
    cache_hit: {
      executor: 'constant-arrival-rate',
      exec: 'cacheHit',
      rate: HIT_RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: VUS,
      maxVUs: VUS * 4,
      startTime: `${DURATION_SECONDS + 5}s`,
    },
  },
  // Thresholds that always pass; they force k6 to export per-scenario sub-metrics.
  thresholds: {
    'http_req_duration{scenario:miss}': ['max>=0'],
    'http_req_duration{scenario:cache_hit}': ['max>=0'],
    'http_req_failed{scenario:miss}': ['rate>=0'],
    'http_req_failed{scenario:cache_hit}': ['rate>=0'],
    'http_reqs{scenario:miss}': ['count>=0'],
    'http_reqs{scenario:cache_hit}': ['count>=0'],
  },
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  // Warm the cache with the whole corpus (one upstream call per prompt on a cold cache).
  for (const p of CORPUS) {
    const res = http.post(URL, payload(p), { headers: HEADERS, timeout: '60s' });
    if (res.status !== 200) {
      fail(`warm-up failed with status ${res.status}: ${String(res.body).slice(0, 200)}`);
    }
  }
  sleep(1); // cache writes may land asynchronously
}

export function miss() {
  const prompt = `miss ${RUN_ID} vu${__VU} it${__ITER}`;
  const res = http.post(URL, payload(prompt), { headers: HEADERS });
  check(res, { 'miss 200': (r) => r.status === 200 });
}

export function cacheHit() {
  const prompt = CORPUS[(__VU * 7919 + __ITER) % CORPUS.length];
  const res = http.post(URL, payload(prompt), { headers: HEADERS });
  check(res, { 'hit 200': (r) => r.status === 200 });
}

function sub(data, name, scenario) {
  const m = data.metrics[`${name}{scenario:${scenario}}`];
  return m ? m.values : null;
}

function scenarioResult(data, scenario) {
  const d = sub(data, 'http_req_duration', scenario);
  const n = sub(data, 'http_reqs', scenario);
  const f = sub(data, 'http_req_failed', scenario);
  if (!d || !n) {
    return null;
  }
  return {
    requests: n.count,
    throughput_rps: DURATION_SECONDS > 0 ? Math.round((n.count / DURATION_SECONDS) * 100) / 100 : n.rate,
    error_rate: f ? f.rate : null,
    latency_ms: { avg: d.avg, p50: d.med, p90: d['p(90)'], p95: d['p(95)'], p99: d['p(99)'], max: d.max },
  };
}

export function handleSummary(data) {
  const result = {
    gateway: GATEWAY,
    base_url: BASE_URL,
    model: MODEL,
    duration_per_scenario: DURATION,
    miss_rate_target: MISS_RATE,
    hit_rate_target: HIT_RATE,
    corpus_size: CORPUS.length,
    dropped_iterations: data.metrics.dropped_iterations ? data.metrics.dropped_iterations.values.count : 0,
    miss: scenarioResult(data, 'miss'),
    cache_hit: scenarioResult(data, 'cache_hit'),
  };
  return {
    [RESULT_FILE]: JSON.stringify(result, null, 2) + '\n',
    stdout: JSON.stringify(result, null, 2) + '\n',
  };
}
