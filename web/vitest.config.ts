import { defineConfig } from "vitest/config";
import path from "node:path";

// Pin a non-UTC zone so any formatting that leaks local time shows up in CI.
process.env.TZ = "America/Chicago";

export default defineConfig({
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "src"),
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./vitest.setup.ts"],
    env: {
      TZ: "America/Chicago",
    },
    include: ["src/**/*.test.{ts,tsx}"],
    coverage: {
      provider: "v8",
      reporter: ["text", "html", "json-summary"],
      reportsDirectory: "./coverage",
      include: ["src/lib/**/*.ts", "src/hooks/**/*.ts", "src/utils/**/*.ts"],
      exclude: ["**/*.test.ts", "**/*.test.tsx", "**/*.d.ts"],
      thresholds: {
        lines: 90,
        statements: 90,
      },
    },
  },
});
