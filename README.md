# Prometheus Metrics - Alert manager - Pager Duty - Integration application.

## Steps:

1. `cd app/ && make build && cd../`
2. `bash start_services.sh <PAGERDUTY_KEY>`
3. For firing service_down alert:
	- `bash scripts/stop_app.sh`
4. Restart the app with `bash scripts/start_app.sh`
5. For firing 404Error alert:
	- `bash scripts/emit_404.sh`

 
