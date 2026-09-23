#!/bin/sh
# shellcheck disable=SC2016 # Variables in sandbox commands expand inside the sandbox.
# Invoked by integration.sh with its disposable root and instance cleanup.
set -eu
binary=$1
root="$2/git-user-config"
mkdir -p "$root/home" "$root/xdg/git" "$root/project"
export HOME="$root/home" XDG_CONFIG_HOME="$root/xdg"
# Keep fixture creation independent of the invoking user's Git configuration.
GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null git init -q "$root/project"
cat >"$HOME/.gitconfig" <<'CONF'
[user]
name = Home Identity
[commit]
gpgSign = false
[alias]
config-marker = config --get user.name
CONF
cat >"$XDG_CONFIG_HOME/git/config" <<'CONF'
[user]
name = XDG Identity
email = inherited@example.invalid
CONF
# Files next to the standard config must remain hidden.
printf 'private sibling\n' >"$XDG_CONFIG_HOME/git/private"
before=$(sha256sum "$HOME/.gitconfig" "$XDG_CONFIG_HOME/git/config")
run_git() {
    "$binary" run --no-config --project "$root/project" \
        --instance integration-git-config --podman off --network none --tty never "$@"
}
run_git --agent-config off /bin/sh -ec '
    test "$(git config-marker)" = "Home Identity"
    test "$(git config --get user.email)" = inherited@example.invalid
    test ! -e "$XDG_CONFIG_HOME/git/private"
    if (printf changed >"$HOME/.gitconfig") 2>/dev/null; then exit 1; fi
    if (printf changed >"$XDG_CONFIG_HOME/git/config") 2>/dev/null; then exit 1; fi
    if git config --global user.name Changed; then exit 1; fi
    git commit --allow-empty -qm "Inherited identity"
    test "$(git log -1 --format="%an <%ae>|%cn <%ce>")" = "Home Identity <inherited@example.invalid>|Home Identity <inherited@example.invalid>"
'
test "$before" = "$(sha256sum "$HOME/.gitconfig" "$XDG_CONFIG_HOME/git/config")"
# Atomic host replacement must be reflected on the next launch.
sed 's/Home Identity/Updated Identity/' "$HOME/.gitconfig" >"$HOME/new-config"
mv "$HOME/new-config" "$HOME/.gitconfig"
for mode in write-through copy-on-write read-only; do
    run_git --workspace-mode "$mode" /bin/sh -ec '
        test "$(git config --get user.name)" = "Updated Identity"
        if (printf changed >"$HOME/.gitconfig") 2>/dev/null; then exit 1; fi
    '
done
# Opt-out permits instance-local config, which subsequent mounts must preserve.
run_git --no-git-config /bin/sh -ec '
    if git config --global --get user.email; then exit 1; fi
    git config --global user.name "Instance Identity"
'
run_git /bin/sh -ec 'test "$(git config --get user.name)" = "Updated Identity"'
run_git --no-git-config /bin/sh -ec 'test "$(git config --get user.name)" = "Instance Identity"'
# Repository configuration keeps its normal precedence over both global files.
git -C "$root/project" config user.name "Repository Identity"
run_git /bin/sh -ec 'test "$(git config --get user.name)" = "Repository Identity"'
"$binary" instance delete integration-git-config --yes
printf 'git-user-config-ok\n'
