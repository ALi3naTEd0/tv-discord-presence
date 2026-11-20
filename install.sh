#!/usr/bin/env bash
# Installer for Linux (systemd user services) and macOS (launchd agents).
# Windows: use install.ps1.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"

case "$(uname -s)" in
  Linux)
    CONF="${XDG_CONFIG_HOME:-$HOME/.config}/tvpresence"
    DATA="${XDG_DATA_HOME:-$HOME/.local/share}/tvpresence"
    ;;
  Darwin)
    CONF="$HOME/Library/Application Support/tvpresence"
    DATA="$CONF"
    ;;
  *) echo "Unsupported OS: $(uname -s). On Windows run install.ps1." >&2; exit 1 ;;
esac

VENV="$DATA/venv"
python3 -c 'import sys; sys.exit(sys.version_info < (3, 11))' || { echo "Python 3.11+ required" >&2; exit 1; }
python3 -m venv "$VENV"
"$VENV/bin/pip" install -q -r "$DIR/requirements.txt"
mkdir -p "$CONF"
[ -f "$CONF/config.toml" ] || cp "$DIR/config.example.toml" "$CONF/config.toml"

if [ "$(uname -s)" = Linux ]; then
  UNITS="$HOME/.config/systemd/user"
  mkdir -p "$UNITS"
  for unit in tvpresence tvhistory; do
    sed "s#%h/tv-discord-presence#$DIR#g" "$DIR/systemd/$unit.service" > "$UNITS/$unit.service"
  done
  systemctl --user daemon-reload
  systemctl --user enable --now tvpresence.service tvhistory.service
  RESTART="systemctl --user restart tvpresence tvhistory"
else
  AGENTS="$HOME/Library/LaunchAgents"
  mkdir -p "$AGENTS"
  for pair in "tvpresence:tvpresence.py" "tvhistory:tvhistory_dashboard.py"; do
    name="${pair%%:*}"; script="${pair#*:}"
    plist="$AGENTS/local.$name.plist"
    cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>local.$name</string>
  <key>ProgramArguments</key><array><string>$VENV/bin/python</string><string>-u</string><string>$DIR/$script</string></array>
  <key>WorkingDirectory</key><string>$DIR</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$DATA/$name.log</string>
  <key>StandardErrorPath</key><string>$DATA/$name.log</string>
</dict></plist>
PLIST
    launchctl bootout "gui/$(id -u)/local.$name" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$plist"
  done
  RESTART="launchctl kickstart -k gui/$(id -u)/local.tvpresence && launchctl kickstart -k gui/$(id -u)/local.tvhistory"
fi

echo "Done."
echo "1. Edit $CONF/config.toml (tv_host, client_id)"
echo "2. Put TMDB_API_KEY=... (and PLEX_TOKEN=... if you use Plex) in $CONF/.env"
echo "3. Restart: $RESTART"
echo "Dashboard: http://127.0.0.1:7790"
