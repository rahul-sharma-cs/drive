import { defineConfig, devices } from '@playwright/test'

// globalSetup: './global-setup.ts' (fixture generation + Mailpit inbox purge)
// is Phase 4 work — not created yet, so left out of this config for now.
export default defineConfig({
  testDir: './tests',
  // spike.spec.ts is driven by `go run ./server/cmd/spike`, which supplies
  // SPIKE_MANIFEST/SPIKE_RESULTS. Without them it cannot run, so keep it out of
  // `make e2e` — but not out of the spike harness's own explicit invocation.
  testIgnore: process.env.SPIKE_MANIFEST ? [] : ['**/spike.spec.ts'],
  workers: 1,
  retries: 0,
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
})
