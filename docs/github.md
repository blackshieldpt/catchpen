# GitHub tutorial

This page sets up a VM that works on one of your GitHub repositories:

- **Part 1, SSH:** `git clone`, `git pull` and `git push` in the VM sign with your own SSH
  key. The key file stays on the host.
- **Part 2, token:** the `gh` command in the VM works (pull requests, issues), with a token
  limited to that one repository.

Part 1 is enough for git. Do Part 2 only if you want `gh` inside the VM.

The examples use the repo `acme/api` and a VM called `dev`; replace them with yours.
Run every command on the host unless it says "in the VM".

## Before you start

curral is installed and `curral doctor` passes ([Installation](installation.md)), and
you've run `curral init`, which creates `~/.config/curral/secrets.env` and the template
category files in `~/.config/curral/templates/` (see [Templates](templates.md)).

## Part 1: SSH

### 1. Check your SSH key works with GitHub

```
ssh -T git@github.com
```

You should see `Hi <your-user>! You've successfully authenticated`. If not, create a key
with `ssh-keygen -t ed25519`, add the contents of `~/.ssh/id_ed25519.pub` on GitHub
under **Settings → SSH and GPG keys → New SSH key**, and try again.

### 2. Tell curral about GitHub

```
curral forge add github --type github --host github.com --mode agent
curral forge ls
```

`forge ls` should list `github`. You do this once per host. `agent` mode means the VM
gets no key of its own: it asks your key, through a restricted agent, to sign.

### 3. Add your key and a profile

Add this section to `~/.config/curral/templates/ssh.yaml`:

```yaml
github-ssh:
  ssh:
    agent: restricted
    agent_keys: [~/.ssh/id_ed25519]   # the key from step 1
    confirm: false                    # see the note below
```

With `confirm: true`, curral asks you to approve each use of the key in a desktop
pop-up (`ssh-askpass`). That needs a display, so on a server keep `confirm: false`.

Then name what this project's VMs get, in `~/.config/curral/templates/profiles.yaml`:

```yaml
acme-api: [github-ssh]
```

A profile is a list of picks; you'll add to it in Part 2, and can add runtimes too
(`acme-api: [github-ssh, go-1.27]`).

### 4. Create the VM with the repo

```
curral up dev -t acme-api --repo github:acme/api
```

If the key has a passphrase, curral asks for it here. The repo is cloned into
`/workspace/api`.

### 5. Work and push

```
curral run dev claude
```

Ask Claude to commit, or commit yourself in the VM, then push in the VM:

```
cd /workspace/api && git push
```

You can also take the commits out from the host instead of pushing from the VM:
`curral pull dev --into ~/src/api` fetches the VM's branch into your host clone.

## Part 2: token for `gh`

The SSH key only covers git. `gh` uses GitHub's web API, which needs a token.

### 1. Create a token for the one repository

On GitHub, open **Settings → Developer settings → Personal access tokens → Fine-grained
tokens → Generate new token**, then:

- **Resource owner:** the repo's owner (`acme`).
- **Repository access:** *Only select repositories* → `acme/api`.
- **Repository permissions:** *Contents*, *Pull requests* and, if you use `gh issue`,
  *Issues*: **Read and write**.
- An expiration date.

Copy the token (it starts with `github_pat_`). If the owner is an organization, an
owner may have to approve the token before it works.

### 2. Store it on the host

Add this line to `~/.config/curral/secrets.env`:

```
GH_TOKEN=github_pat_...
```

### 3. Pass it to the VM

Add this section to `~/.config/curral/templates/env.yaml`:

```yaml
gh-token:
  env: { GH_TOKEN: "secret:GH_TOKEN" }
```

and add it to the profile in `profiles.yaml`:

```yaml
acme-api: [github-ssh, gh-token]
```

`secret:GH_TOKEN` tells curral to read the value from `secrets.env` on the host; the
category files hold no secret.

### 4. Push it to the VM

New VMs made with `-t acme-api` get the token when they're created. The VM from Part 1 picked
the same profile, so it gets it too:

```
curral creds push dev
```

It prints `dev: env from template acme-api: +GH_TOKEN`. New sessions get the token; a Claude
Code session that was already open doesn't, so restart the VM to be sure:

```
curral stop dev && curral start dev
```

### 5. Check it, in the VM

```
gh auth status
```

It should say you're logged in to github.com with the `GH_TOKEN` token.

When the token expires, put the new one in `secrets.env` and run `curral creds push dev`
again.

## Good to know

- Anything in the VM, Claude included, can use the token, so keep it to one repo and
  give it an expiry. The SSH key is safer: the VM can only ask it to sign for
  github.com, through that VM, and only while the VM runs.
- Don't want the VM to hold a token at all? Leave out Part 2 and open pull requests from
  the host with `curral pr dev --into ~/src/api`. That needs a token on the forge
  (`curral forge add ... --token-secret NAME`) but not in the VM.
- Don't want to use your own key? Use `--mode deploy-key` in step 2 of Part 1 (with
  `--token-secret` for a token that can manage deploy keys): each VM then gets its own
  key for the repo, removed when the VM is destroyed. See [Forges](forges.md#modes).

## If something goes wrong

| You see | Do this |
|---|---|
| `no forge for host github.com` | Part 1, step 2: `curral forge add ...` was not run on this host. |
| `Permission denied (publickey)` on clone or push | Run `ssh -T git@github.com` on the host (step 1), and check `agent_keys` names that key. |
| `the restricted agent refused` | `confirm: true` on a host without a display: set `confirm: false` in `ssh.yaml` and create the VM again (a VM keeps the `ssh:` settings it was created with). |
| `gh: To get started with GitHub CLI, please run: gh auth login` | Part 2: run `curral creds push dev`, then open a new session (or restart the VM). |
| `gh` says `HTTP 403` or `Resource not accessible` | The token lacks a permission or the repo (Part 2, step 1), or an organization owner hasn't approved it. |
