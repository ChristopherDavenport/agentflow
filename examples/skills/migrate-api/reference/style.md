# House style for migrations

- One package per change, in an order that builds after each one.
- A migration changes call sites only. A signature that differs is
  adapted at the call, never by a wrapper in the old package.
- Tests are not edited to pass. A test that fails after the change is a
  finding.
