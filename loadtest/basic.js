// Continuous background load per CLAUDE.md §2.4/§3 ("k6 sinh tải nền
// 20–50 rps"). Runs indefinitely at a constant arrival rate so that
// whatever fault-controller injects has real, ongoing traffic to show
// up in — a scenario's `wait` step alone generates zero requests.
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
  http.get('http://gateway.demo:8080/create?user_id=1&id=1', { timeout: '10s' });
}
