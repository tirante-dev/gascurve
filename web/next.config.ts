import type { NextConfig } from "next";
import pkg from "./package.json";

// The mock flag is inlined as a literal at build time so that the
// `if (process.env.NEXT_PUBLIC_USE_MOCK_DATA === "true")` branch in the API
// client is constant-folded away in production builds. Without this the
// bundler would keep a runtime check and ship the mock world as a lazy chunk.
const useMockData = process.env.NEXT_PUBLIC_USE_MOCK_DATA === "true" ? "true" : "false";

const nextConfig: NextConfig = {
  output: "standalone",
  reactStrictMode: true,
  env: {
    NEXT_PUBLIC_USE_MOCK_DATA: useMockData,
    NEXT_PUBLIC_APP_VERSION: pkg.version,
  },
};

export default nextConfig;
