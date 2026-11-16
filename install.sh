#!/usr/bin/env bash
set -Eeuo pipefail

REPO="stonercraft420/BedrockNodeTunnel"
RAW_BASE="https://raw.githubusercontent.com/$REPO/main"
RELEASE_BASE="https://github.com/$REPO/releases/latest/download"
ACTION="install"
if [[ $# -gt 0 ]]; then ACTION="$1"; shift; fi
PURGE_DATA=0

for arg in "$@"; do
  case "$arg" in
    --purge-data) PURGE_DATA=1 ;;
    *)
      echo "Unknown option: $arg" >&2
      echo "Usage: $0 [install|update|uninstall] [--purge-data]" >&2
      exit 2
      ;;
  esac
done

case "$ACTION" in
  install|update|uninstall) ;;
  *)
    echo "Usage: $0 [install|update|uninstall] [--purge-data]" >&2
    exit 2
    ;;
esac

kernel="$(uname -s 2>/dev/null || echo unknown)"
case "$kernel" in
  Linux*) PLATFORM="linux" ;;
  MINGW*|MSYS*|CYGWIN*) PLATFORM="windows" ;;
  *)
    echo "Unsupported operating system: $kernel" >&2
    echo "Supported systems: Linux and Windows (Git Bash/MSYS/Cygwin delegates to PowerShell)." >&2
    exit 1
    ;;
esac

download() {
  local url="$1"
  local output="$2"
  if command -v curl >/dev/null 2>&1; then
    curl --fail --location --silent --show-error --retry 3 "$url" --output "$output"
  elif command -v wget >/dev/null 2>&1; then
    wget --tries=3 --output-document="$output" "$url"
  else
    echo "curl or wget is required." >&2
    return 1
  fi
}

if [[ "$PLATFORM" == "windows" ]]; then
  powershell=""
  if command -v powershell.exe >/dev/null 2>&1; then
    powershell="powershell.exe"
  elif command -v pwsh.exe >/dev/null 2>&1; then
    powershell="pwsh.exe"
  else
    echo "PowerShell was not found. Run install.ps1 from PowerShell instead." >&2
    exit 1
  fi

  tmp_dir="$(mktemp -d)"
  trap 'rm -rf "$tmp_dir"' EXIT
  script_path="${BASH_SOURCE[0]:-}"
  if [[ -n "$script_path" && -f "$script_path" ]]; then
    script_dir="$(cd "$(dirname "$script_path")" 2>/dev/null && pwd)"
    cp "$script_dir/install.ps1" "$tmp_dir/install.ps1"
  else
    download "$RAW_BASE/install.ps1" "$tmp_dir/install.ps1"
  fi

  ps_file="$tmp_dir/install.ps1"
  if command -v cygpath >/dev/null 2>&1; then
    ps_file="$(cygpath -w "$ps_file")"
  fi

  if [[ "$PURGE_DATA" -eq 1 ]]; then
    "$powershell" -NoProfile -ExecutionPolicy Bypass -File "$ps_file" -Action "$ACTION" -PurgeData
  else
    "$powershell" -NoProfile -ExecutionPolicy Bypass -File "$ps_file" -Action "$ACTION"
  fi
  exit $?
fi

if [[ ! -v HOME || -z "$HOME" ]]; then
  echo "HOME is not set; cannot choose an installation directory." >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "Unsupported Linux architecture: $(uname -m). Supported: x86_64 and aarch64." >&2
    exit 1
    ;;
esac

INSTALL_DIR="$HOME/.local/bin"
if [[ -v BNT_INSTALL_DIR && -n "$BNT_INSTALL_DIR" ]]; then INSTALL_DIR="$BNT_INSTALL_DIR"; fi
CONFIG_HOME="$HOME/.config"
if [[ -v XDG_CONFIG_HOME && -n "$XDG_CONFIG_HOME" ]]; then CONFIG_HOME="$XDG_CONFIG_HOME"; fi
SERVICE_DIR="$CONFIG_HOME/systemd/user"
SERVICE_FILE="$SERVICE_DIR/bedrocknode-tunnel.service"
BINARY_PATH="$INSTALL_DIR/nethernet-client"
DATA_DIR="$CONFIG_HOME/bedrocknode-tunnel"

if [[ "$ACTION" == "uninstall" ]]; then
  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user disable --now bedrocknode-tunnel.service >/dev/null 2>&1 || true
  fi
  rm -f "$SERVICE_FILE" "$BINARY_PATH"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user daemon-reload >/dev/null 2>&1 || true
  fi
  rmdir "$INSTALL_DIR" 2>/dev/null || true
  if [[ "$PURGE_DATA" -eq 1 ]]; then
    rm -rf "$DATA_DIR"
    echo "Removed the saved tunnel token and node configuration."
  else
    echo "Kept saved tunnel data in: $DATA_DIR"
  fi
  echo "nethernet-client has been uninstalled."
  exit 0
fi

ASSET="nethernet-client-linux-$ARCH"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

echo "Downloading latest Linux/$ARCH build..."
download "$RELEASE_BASE/$ASSET" "$tmp_dir/$ASSET"
download "$RELEASE_BASE/SHA256SUMS" "$tmp_dir/SHA256SUMS"

expected="$(awk -v name="$ASSET" '$2 == name || $2 == "*" name { print $1; exit }' "$tmp_dir/SHA256SUMS")"
if [[ -z "$expected" ]]; then
  echo "No SHA-256 entry found for $ASSET; refusing to install an unverified build." >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$tmp_dir/$ASSET" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$tmp_dir/$ASSET" | awk '{print $1}')"
else
  echo "sha256sum or shasum is required to verify the download." >&2
  exit 1
fi

expected="$(printf '%s' "$expected" | tr 'A-F' 'a-f')"
actual="$(printf '%s' "$actual" | tr 'A-F' 'a-f')"
if [[ "$actual" != "$expected" ]]; then
  echo "SHA-256 verification failed for $ASSET." >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR"
chmod 0755 "$tmp_dir/$ASSET"
mv -f "$tmp_dir/$ASSET" "$BINARY_PATH"

# Install a per-user systemd unit when systemd is available.
if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  mkdir -p "$SERVICE_DIR"
  if [[ "$BINARY_PATH" == *" "* ]]; then
    echo "Skipping systemd unit because the installation path contains spaces."
  else
    cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Bedrock Node Tunnel client
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$BINARY_PATH
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOF
    systemctl --user daemon-reload >/dev/null 2>&1 || true
    echo "User service installed. Enable it with:"
    echo "  systemctl --user enable --now bedrocknode-tunnel.service"
  fi
fi

echo "Installed nethernet-client to: $BINARY_PATH"
echo "Saved tokens and node settings are preserved in: $DATA_DIR"
if [[ ":$PATH:" != *":$INSTALL_DIR:"* ]]; then
  echo "Add this directory to PATH to run it from any terminal:"
  echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
fi
if [[ "$ACTION" == "update" ]]; then
  echo "nethernet-client has been updated."
else
  echo "Installation complete. Run: \"$BINARY_PATH\""
fi