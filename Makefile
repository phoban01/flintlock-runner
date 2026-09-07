DUVET ?= duvet

.PHONY: duvet duvet-ci duvet-open

## duvet: extract requirements, build the HTML/JSON report and refresh the snapshot
duvet:
	rm -rf .duvet/requirements
	$(DUVET) report

## duvet-ci: same as duvet but fail if .duvet/snapshot.txt would change
duvet-ci:
	rm -rf .duvet/requirements
	$(DUVET) report --ci

## duvet-open: build the report and open it in a browser
duvet-open: duvet
	xdg-open .duvet/reports/report.html 2>/dev/null || open .duvet/reports/report.html
