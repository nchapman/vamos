# Fixed bootstrap collector. Never source os-release or interpolate user input.
set -eu
export LC_ALL=C
printf '%s\n' 'soar-inspect-v1'
uname -s
uname -m
id -u
if [ -r /proc/1/comm ]; then
  init=''
  IFS= read -r init < /proc/1/comm || [ -n "$init" ]
  printf '%s\n' "$init"
else
  printf '%s\n' 'unknown'
fi
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
  printf '%s\n' 'yes'
else
  printf '%s\n' 'no'
fi
if [ -r /etc/os-release ]; then
  printf '%s\n' 'present'
  # An extra byte lets the parser distinguish a bounded read from truncation.
  dd if=/etc/os-release bs=16385 count=1 2>/dev/null
else
  printf '%s\n' 'missing'
fi
