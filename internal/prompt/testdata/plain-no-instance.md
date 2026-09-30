# KubePodCrashLooping

- **Alertmanager URL:** https://alertmanager.example.com
- **Severity:** critical
- **State:** Active
- **Started:** 2026-09-28T09:05:00Z
- **Receivers:** team-core, pager
- **Source:** https://grafana.example.com/alerting/grafana/abc/view

## Summary

Pod core/api-x-7d9c is crash looping.

## Description

Pod core/api-x-7d9c (api) is restarting 2.1 times / 10 minutes.

## Labels

- `alertname`: `KubePodCrashLooping`
- `container`: `api`
- `namespace`: `core`
- `pod`: `api-x-7d9c`
- `severity`: `critical`

## Annotations

- `dashboard`: `https://grafana.example.com/d/abc`
- `runbook_url`: `https://runbooks.example.com/KubePodCrashLooping`
