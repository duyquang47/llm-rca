// Continuous background load per CLAUDE.md §2.4/§3 ("k6 sinh tải nền
// 20–50 rps"). Runs indefinitely at a constant arrival rate so that
// whatever fault-controller injects has real, ongoing traffic to show
// up in — a scenario's `wait` step alone generates zero requests.
//
// user_id is randomized across a wide range (not a fixed "1") so
// order's cache (internal/chain/handler.go's cacheGet/cacheSet,
// case #7's real cache-aside short-circuit) sees a near-zero baseline
// hit rate — at 20rps with keys this spread out, the same key almost
// never repeats within the 60s TTL. This keeps every other scenario's
// traffic-to-inventory volume essentially unchanged from before cache
// hits could skip downstream at all; case #7 relies on scaling redis
// to 0 (not on the ambient hit rate) for its signal.
import http from 'k6/http';

export const options = {
  scenarios: {
    background_load: {
      executor: 'constant-arrival-rate',
      rate: 20,
      timeUnit: '1s',
      duration: '24h',
      preAllocatedVUs: 10,
      maxVUs: 50,
    },
  },
};

export default function () {
  const userID = Math.floor(Math.random() * 1000000) + 1;
  http.get(`http://gateway.demo:8080/create?user_id=${userID}&id=1`, { timeout: '10s' });
}
