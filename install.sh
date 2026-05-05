#!/bin/sh
set -e

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
esac

DEST="$HOME/.obmon/bin/obmon"
mkdir -p "$HOME/.obmon/bin"

URL="https://github.com/omnibenchmark/obmon/releases/download/nightly/obmon_${OS}_${ARCH}"
echo "Downloading obmon..."
curl -fsSL "$URL" -o "$DEST"
chmod +x "$DEST"
echo "Installed: $DEST"

# Append PATH export to a shell rc file, with permission.
add_to_path() {
  RC="$1"
  LINE='export PATH="$HOME/.obmon/bin:$PATH"'

  # Skip if already present.
  if [ -f "$RC" ] && grep -qF '.obmon/bin' "$RC"; then
    echo "$RC: PATH entry already present, skipping."
    return
  fi

  # Open /dev/tty read-only as fd 3 so read gets keyboard input even when
  # stdin is a pipe (curl | sh).  printf goes to stdout which is still the
  # terminal in that case.
  exec 3</dev/tty 2>/dev/null || {
    echo "No terminal — add to $RC manually:"
    echo "  $LINE"
    return
  }

  printf 'Add %s to PATH in %s? [y/N] ' '$HOME/.obmon/bin' "$RC"
  read -r REPLY <&3 || REPLY=''
  exec 3<&-
  case "$REPLY" in
    [yY]|[yY][eE][sS])
      printf '\n# obmon\n%s\n' "$LINE" >> "$RC"
      echo "Updated $RC."
      ;;
    *)
      echo "Skipped $RC."
      ;;
  esac
}

# Detect active shell and offer the matching rc file.
SHELL_NAME=$(basename "${SHELL:-sh}")
case "$SHELL_NAME" in
  zsh)  add_to_path "$HOME/.zshrc" ;;
  bash) add_to_path "$HOME/.bashrc" ;;
  *)
    echo "Unknown shell ($SHELL_NAME). Add the following to your shell rc manually:"
    echo '  export PATH="$HOME/.obmon/bin:$PATH"'
    ;;
esac

echo ""
echo "Run 'obmon --help' after opening a new shell (or: source the rc file above)."
