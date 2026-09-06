# Security Policy

## Reporting a vulnerability

Please do not open a public issue for security problems. Use GitHub's private vulnerability reporting on this repository ("Security" tab, "Report a vulnerability"). You will get an acknowledgement within a few days.

## Scope

This project reads public blockchain data and serves it over HTTP and WebSocket. The interesting surface is the API server (`cmd/api`) and the web app (`web/`). The collector only talks to configured RPC endpoints and the database.

## Supported versions

Only the latest release receives fixes.
