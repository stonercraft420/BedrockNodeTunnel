# ⚡ Bedrock Node Tunnel (Open-Source Client)

![Bedrock Node Tunnel](logo.jpg)

**Bedrock Node Tunnel** is a 100% Vanilla Minecraft Bedrock Dedicated Server (`transport=nethernet`) WebRTC UDP + TCP tunneling client with a **Raw UDP Fast-Lane**. Host your Bedrock server behind Starlink, CGNAT, or strict routers without WaterdogPE, Geyser, or wiped player XUIDs/inventories.

* 🌐 **Get Your Free Tunnel & Ports:** [https://bedrocknode.net](https://bedrocknode.net)
* 💬 **Join Our Discord:** [https://discord.gg/Fcq2yCjQnV](https://discord.gg/Fcq2yCjQnV)
* 💻 **Official Repository:** [https://github.com/stonercraft420/BedrockNodeTunnel](https://github.com/stonercraft420/BedrockNodeTunnel)

---

## 🔍 Why Is This Repository Open-Source?
We believe server hosts should never have to run mystery `.exe` or binary files on their machines. This repository contains the complete source code (`client.go`) for `nethernet-client`:
* **Zero File or System Access:** Only forwards traffic between the Bedrock Node Tunnel regional hubs (`US`, `TX`, `UK`, `BR`, `ASIA`) and your local Bedrock server (`127.0.0.1:19132` + local WebRTC UDP ports).
* **Single Official Dependency:** Uses only the standard Go library and `github.com/hashicorp/yamux`. Release builds are statically linked (`CGO_ENABLED=0`) and verified via `SHA256SUMS`.

> 🔒 **Security & Official Support Notice:** Always make sure your install URLs and downloads point to `stonercraft420/BedrockNodeTunnel`. We cannot verify, audit, or provide support for scripts or binaries hosted on third-party forks, which may fall behind on critical protocol updates.

---

## 🚀 Quick Start (Windows & Linux)

### Option A: Windows Quick Start (PowerShell or Pre-Built `.exe`)

**1. One-Line PowerShell Installer / Updater (Windows 10/11):**
Open PowerShell and run:
```powershell
irm https://raw.githubusercontent.com/stonercraft420/BedrockNodeTunnel/main/install.ps1 | iex
```

**2. Direct `.exe` Download (Alternative):**
Download `nethernet-client.exe` from [https://bedrocknode.net](https://bedrocknode.net) (or the [Releases](../../releases) tab) and double-click it.
* *Note: If Windows SmartScreen says "Windows protected your PC", click **More info** -> **Run anyway**.*

**3. Link Your Tunnel:**
1. Run `nethernet-client` in PowerShell (or double-click `nethernet-client.exe`).
2. Click the **Claim Link** shown in the console to link the tunnel to your free account at [https://bedrocknode.net](https://bedrocknode.net).
3. Copy the 4 `server.properties` lines shown on your dashboard into your Bedrock Dedicated Server and restart `bedrock_server.exe`.

---

### Option B: Linux Quick Start (Ubuntu, Debian, Arch, Any Distro)

#### 1. One-Line Auto-Installer & Updater (Recommended)
Works on any `x86_64` (`amd64`) or `aarch64` (`arm64`) Linux distribution, verifies the `SHA256SUMS` checksum automatically, and sets up a per-user `systemd` service:

* **Fresh Install (or Update to Latest Release):**
  ```bash
  curl -fsSL https://raw.githubusercontent.com/stonercraft420/BedrockNodeTunnel/main/install.sh | bash
  ```

* **Explicit Update Command:**
  ```bash
  curl -fsSL https://raw.githubusercontent.com/stonercraft420/BedrockNodeTunnel/main/install.sh | bash -s -- update
  ```

* **First Run (Claim Your Tunnel):**
  ```bash
  ~/.local/bin/nethernet-client
  ```
  *(Once you claim your tunnel link in the terminal, press `Ctrl+C` and start the background service below.)*

---

#### 2. Managing the Linux Background Service (`systemd`)
If you installed via `install.sh` or the `.deb` package, the client is configured as a user-level `systemd` service (`bedrocknode-tunnel.service`):

* **Enable auto-start on boot and start immediately:**
  ```bash
  systemctl --user enable --now bedrocknode-tunnel.service
  ```

* **Keep tunnel running after closing SSH (Required for headless Linux servers):**
  ```bash
  sudo loginctl enable-linger "$USER"
  ```

* **Check tunnel status:**
  ```bash
  systemctl --user status bedrocknode-tunnel.service
  ```

* **View live tunnel logs (see your assigned public address or debug issues):**
  ```bash
  journalctl --user -u bedrocknode-tunnel.service -f
  ```

* **Restart or stop the tunnel:**
  ```bash
  systemctl --user restart bedrocknode-tunnel.service
  systemctl --user stop bedrocknode-tunnel.service
  ```

---

#### 3. Uninstalling or Clean Reinstalling (Linux)

* **Uninstall client (keeps your saved tunnel token and node settings):**
  ```bash
  curl -fsSL https://raw.githubusercontent.com/stonercraft420/BedrockNodeTunnel/main/install.sh | bash -s -- uninstall
  ```

* **Complete Uninstall / Purge (removes client AND wipes saved token/configs):**
  ```bash
  curl -fsSL https://raw.githubusercontent.com/stonercraft420/BedrockNodeTunnel/main/install.sh | bash -s -- uninstall --purge-data
  ```

---

#### 4. Manual Package / Binary Installation (Linux Alternatives)
If you prefer not to use the one-line installer script, download the packages from the [Releases](../../releases/latest) page:

* **Debian / Ubuntu / Raspberry Pi OS (`.deb`):**
  ```bash
  sudo apt install ./bedrocknode-tunnel_1.3_amd64.deb
  ```

* **Arch Linux (`PKGBUILD`):**
  Download `PKGBUILD` and the source tarball into one folder, then run:
  ```bash
  makepkg -si
  ```

* **Standalone Linux Binary (Direct Download):**
  ```bash
  sudo curl -fsSL https://github.com/stonercraft420/BedrockNodeTunnel/releases/latest/download/nethernet-client-linux-amd64 -o /usr/local/bin/nethernet-client && sudo chmod +x /usr/local/bin/nethernet-client
  ```

---

## 🧰 Linux Troubleshooting & Fallbacks

* **"Text File Busy" / Service Running During Manual Update:**
  If a manual install cannot overwrite the binary while it is running in the background, stop the user service and kill any existing process first:
  ```bash
  systemctl --user stop bedrocknode-tunnel.service 2>/dev/null; killall nethernet-client 2>/dev/null
  ```

* **Clearing Broken `.deb` Package States (From Older v1.2 Builds):**
  If `apt` complains about a broken package header or merge list from an older `.deb` file, clear the package state and remove the corrupt `.deb` file before installing v1.3+:
  ```bash
  sudo dpkg --remove --force-remove-reinstreq bedrocknode-tunnel 2>/dev/null
  ```

---

## 🛠️ Build or Run From Source Yourself
If you prefer to compile the client yourself from source:

1. Install [Go 1.21+](https://go.dev/dl/).
2. Clone this repository:
   ```bash
   git clone https://github.com/stonercraft420/BedrockNodeTunnel.git
   cd BedrockNodeTunnel
   ```
3. Run directly or build a binary:
   ```bash
   go run client.go
   # OR build an executable:
   go build -o nethernet-client client.go
   ```

### Useful CLI Flags
* `-version` — Prints the current client version.
* `-reset-token` — Forgets the saved tunnel token and generates a new claim link.
* `-token <value>` — Authenticates using a specific token directly (skips the interactive claim step).
