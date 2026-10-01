# winpipe

A lightweight TCP bridge from WSL to Windows over stdio.

Run your tools in WSL and reach Windows-local services without changing WSL NAT
networking, opening firewall ports, or installing an SSH server. WSL interop must
be enabled: the Linux listener launches a Windows executable for each connection.

```text
WSL TCP client -> Linux winpipe -> stdin/stdout -> Windows winpipe.exe -> Windows TCP service
```

## Build

Requires Go 1.22 or newer on the build machine. Windows needs no Go installation.
For x64 Linux and Windows, run from the project directory:

```sh
mkdir -p dist
GOOS=linux GOARCH=amd64 go build -o dist/winpipe .
GOOS=windows GOARCH=amd64 go build -o dist/winpipe.exe .
go test -race ./...
```

Use `GOARCH=arm64` instead for ARM64 machines. The race detector additionally
requires a C compiler in WSL; without one, run `go test ./...`.

The Windows executable can stay in this project: WSL interop can launch it
directly from the Linux filesystem. Alternatively, copy it to a Windows directory
you own (for example `C:\Users\<you>\Tools\winpipe.exe`).

## Forward a port

Run in WSL:

```sh
./dist/winpipe listen \
  --listen 127.0.0.1:8080 \
  --target 127.0.0.1:3000 \
  --helper "$(pwd)/dist/winpipe.exe"
```

WSL's `localhost:8080` now reaches Windows's `localhost:3000`. The target is
resolved on Windows. Each accepted connection gets its own helper process;
HTTP, HTTPS, WebSockets, SSH, databases, and other TCP protocols can pass through.
Multiple concurrent connections are supported. Stop the listener with Ctrl+C.

The listener only accepts literal loopback addresses, including `[::1]:8080`.
The connection timeout defaults to 10 seconds (`--dial-timeout 30s` to change it).
There is no idle timeout, so long-lived WebSockets stay open.

## Windows Chrome with a Linux MCP server

1. Launch a separate Chrome instance from Windows PowerShell:

   ```powershell
   & "$env:ProgramFiles\Google\Chrome\Application\chrome.exe" `
     --remote-debugging-port=9222 `
     "--user-data-dir=$env:LOCALAPPDATA\winpipe\chrome-profile"
   ```

   Adjust the Chrome executable path if needed. Keep the debugging listener on
   loopback. Chrome requires a non-default user-data directory for this launch.
   Do not use this profile for sensitive browsing: local processes and the agent
   can control it.

2. Start the bridge in WSL:

   ```sh
   ./dist/winpipe listen \
     --listen 127.0.0.1:9222 \
     --target 127.0.0.1:9222 \
     --helper "$(pwd)/dist/winpipe.exe"
   ```

3. Verify discovery from WSL:

   ```sh
   curl http://127.0.0.1:9222/json/version
   ```

4. Run the MCP server using Linux Node.js:

   ```sh
   npx -y chrome-devtools-mcp@latest \
     --browser-url=http://127.0.0.1:9222 \
     --no-usage-statistics
   ```

   For OpenCode V2, merge this into your configuration:

   ```jsonc
   {
     "$schema": "https://opencode.ai/config.json",
     "mcp": {
       "servers": {
         "chrome-devtools": {
           "type": "local",
           "command": [
             "npx", "-y", "chrome-devtools-mcp@latest",
             "--browser-url=http://127.0.0.1:9222",
             "--no-usage-statistics"
           ]
         }
       }
     }
   }
   ```

Using port 9222 on both sides lets Chrome's advertised loopback WebSocket URL
point through the bridge without rewriting. For other ports or unusual advertised
addresses, use the MCP server's `--ws-endpoint` with the WSL listening address
and the browser path from `/json/version`.

## Limits

- TCP only; no UDP, reverse forwarding, or automatic service startup.
- Chrome and the bridge must be running before browser tools connect.
- This is a byte relay, not a sandbox or an authentication layer.
- Client write-half-close is forwarded; destination read EOF ends the session.
  Protocols that require sending more client data after destination EOF are not
  supported by this initial version.
- Diagnostics go to stderr; helper stdout contains only forwarded bytes.
- Real WSL-to-Windows forwarding has been smoke-tested against a Windows TCP
  server, including binary payloads and client half-close. Chrome/MCP connectivity
  still needs its own end-to-end test.
