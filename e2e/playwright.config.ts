import { defineConfig, devices } from '@playwright/test'

// globalSetup: './global-setup.ts' (fixture generation + Mailpit inbox purge)
// does not exist — each spec makes its own fixtures — so it is left out here.

// Two specs are driven by something outside `make e2e` and stay out of it
// unless that something says otherwise:
//   spike.spec.ts  — driven by `go run ./server/cmd/spike`, which supplies
//                    SPIKE_MANIFEST/SPIKE_RESULTS; without them it cannot run.
//   resume.spec.ts — owns the server process (it kills it mid-upload on
//                    purpose), so it cannot share `make e2e`'s single server.
//                    `make e2e-resume` sets DRIVE_RESUME_SPEC and runs it alone.
const excluded = [
  ...(process.env.SPIKE_MANIFEST ? [] : ['**/spike.spec.ts']),
  ...(process.env.DRIVE_RESUME_SPEC ? [] : ['**/resume.spec.ts']),
]

export default defineConfig({
  testDir: './tests',
  testIgnore: excluded,
  workers: 1,
  retries: 0,
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
})
