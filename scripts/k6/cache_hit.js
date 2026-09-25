// k6 load test for the Tier-1 exact-cache hit path (M10.3 target: 5000 req/s, P99 < 5 ms).
//
// Run against a started gateway:
//   k6 run scripts/k6/cache_hit.js
//   k6 run -e BASE_URL=http://localhost:8080 -e RATE=5000 -e DURATION=30s scripts/k6/cache_hit.js
//
// setup() sends the request once so the reply is cached (one upstream call if the entry is not
// cached yet), then aborts unless the next response is a Tier-1 hit. The load phase only measures
// cache hits; any miss fails the "tier-1 hit" check and the run's thresholds.
//
// Environment:
//   BASE_URL  gateway address (default http://localhost:8080)
//   PROTOCOL  openai or anthropic (default openai)
//   MODEL     model name in the request (default gpt-4o-mini / claude-haiku-4-5-20251001)
//   PROMPT    user message (default a fixed benchmark prompt)
//   RATE      requests per second (default 5000)
//   DURATION  load duration (default 30s)
//   VUS       pre-allocated virtual users (default 200)

import http from 'k6/http';
import { check, fail, sleep } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const PROTOCOL = (__ENV.PROTOCOL || 'openai').toLowerCase();
const PROMPT = __ENV.PROMPT || 'liltok k6 cache-hit benchmark: reply with the word ok.';
const RATE = parseInt(__ENV.RATE || '5000', 10);
const DURATION = __ENV.DURATION || '30s';
const VUS = parseInt(__ENV.VUS || '200', 10);

const request = PROTOCOL === 'anthropic'
  ? {
      url: `${BASE_URL}/v1/messages`,
      body: JSON.stringify({
        model: __ENV.MODEL || 'claude-haiku-4-5-20251001',
        max_tokens: 16,
        temperature: 0,
        messages: [{ role: 'user', content: PROMPT }],
      }),
      headers: { 'Content-Type': 'application/json', 'anthropic-version': '2023-06-01' },
    }
  : {
      url: `${BASE_URL}/v1/chat/completions`,
      body: JSON.stringify({
        model: __ENV.MODEL || 'gpt-4o-mini',
        temperature: 0,
        messages: [{ role: 'user', content: PROMPT }],
      }),
      headers: { 'Content-Type': 'application/json' },
    };

export const options = {
  scenarios: {
    cache_hits: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: VUS,
      maxVUs: VUS * 4,
    },
  },
  thresholds: {
    'http_req_duration{scenario:cache_hits}': ['p(99)<5'],
    'checks{scenario:cache_hits}': ['rate>0.999'],
    dropped_iterations: ['count<' + Math.ceil(RATE * 0.01)],
  },
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

function isTier1Hit(res) {
  return res.status === 200
    && res.headers['X-Liltok-Cache-Status'] === 'HIT'
    && res.headers['X-Liltok-Cache-Tier'] === 'TIER1_EXACT';
}

export function setup() {
  const warm = http.post(request.url, request.body, { headers: request.headers, timeout: '120s' });
  if (warm.status !== 200) {
    fail(`warm-up request returned ${warm.status}: ${String(warm.body).slice(0, 300)}`);
  }
  for (let i = 0; i < 20; i++) {
    const res = http.post(request.url, request.body, { headers: request.headers });
    if (isTier1Hit(res)) {
      return;
    }
    sleep(0.1); // the cache write after a miss can land asynchronously
  }
  fail('the warmed request was not served as a Tier-1 hit; check that exact caching is enabled');
}

export default function () {
  const res = http.post(request.url, request.body, { headers: request.headers });
  check(res, { 'tier-1 hit': isTier1Hit });
}
