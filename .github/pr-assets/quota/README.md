HQ quota card visual evidence (headless Chromium, local `httptest` HQ page with populated Activity and Idea inbox):

- `prompt-*`: no READ token stored; the other HQ panels still render.
- `ok-*`: dashboard-owned view of the vendored `quota.json` fixture.
- `unavailable-*`: simulated HTTP 500 from the quota endpoint, isolated to the card.

Desktop viewport: 1440 × 1000. Mobile viewport: 390 × 844, scrolled to the card. The browser receives a test-only session; no real credentials or live data appear in these images.
