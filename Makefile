# Develop on the Mac, push to GitHub, then update the desktop from git.
DESKTOP ?= desktop
UPDATE = C:\Users\Admin\projects\homebase\scripts\update.ps1

.PHONY: test windows update

test:
	go vet ./...
	GOOS=windows go vet ./...
	go test -race ./...

# Cross-compile, just to check the Windows build (the desktop builds its own).
windows:
	mkdir -p dist
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o dist/homebase.exe .

# After `git push`: make update APP=homebase   (or APP=sentinel, APP=jobtracker)
update:
	@test -n "$(APP)" || (echo "usage: make update APP=<name>"; exit 1)
	ssh $(DESKTOP) "powershell -NoProfile -ExecutionPolicy Bypass -File $(UPDATE) -App $(APP)"
