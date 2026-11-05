# Sandbox shell (`sandbox_exec`)

An Agent with `spec.shell` gets a built-in `sandbox_exec` tool that runs shell commands in
an isolated [OpenShell](openshell-sandbox.md) sandbox. The model asks for a command; the
operator creates the run's sandbox on the first call, runs the command, and deletes the
sandbox when the run ends.

This is different from `spec.runtime.sandbox`, which runs the agent *process* inside a
sandbox. Here the agent stays in its normal pod and **only the commands it asks for** run
in the sandbox. Use it to let an agent build, test, run scripts and inspect things without
giving the agent process itself a shell.

## Enable it

```yaml
spec:
  shell:
    classRef: coder                                   # required: governs the sandbox
    image: ghcr.io/heddles/agent-orca/dev-sandbox:1   # required: the toolchains the agent gets
    workDir: /sandbox                                 # default
    maxTimeoutSeconds: 600                            # default; at most 3600
```

Prerequisites:

- The OpenShell gateway is installed and the operator is configured for it
  ([setup](openshell-sandbox.md#prerequisites)). Without it the tool answers that the
  operator has no gateway.
- The Agent's namespace is labelled `agentorca.io/enable-sandboxes: "true"`.
- A [SandboxClass](openshell-sandbox.md#sandbox-classes) exists and is available to the namespace.
- The image comes from an allowed registry (`ALLOWED_REGISTRIES`).

Admission checks the namespace, the class and the image when the Agent is created.

See [`config/samples/agent-shell.yaml`](../config/samples/agent-shell.yaml).

## What the agent sees

One tool, `sandbox_exec`, with three arguments:

| Argument | |
|---|---|
| `command` | Required. A shell script, run with `sh`. |
| `work_dir` | Directory to run in. Default: `spec.shell.workDir`. |
| `timeout_seconds` | Default 120, capped by `spec.shell.maxTimeoutSeconds`. A command that outlives it is stopped. |

The result is JSON: `exit_code` and `output`, the tail (about 12 KB) of stdout and stderr
together. A command that exits non-zero is a normal result; the model reads the exit code.
`error` is set when the command did not run or was killed (for example by an out-of-memory
kill), and `timed_out: true` when it hit its timeout.

**The sandbox persists for the run.** Files written and tools installed in one call are still
there in the next, so the agent can clone, build, then test. Each command is a separate
`sh`: shell state such as `cd` and exported variables does not carry over, but the
filesystem does.

## Who governs what

| | Decided by |
|---|---|
| Network egress, filesystem paths, resources, node placement, minimum enforcement | The **SandboxClass** only. The Agent cannot widen it. |
| Credentials (a GitHub token, API keys) | The **SandboxClass** only: its [`credentials`](openshell-sandbox.md#class-credentials). The Agent cannot add one. |
| Whether the Agent has a shell | `spec.shell` on the Agent, and the namespace opt-in label |
| Which image | `spec.shell.image`, subject to `ALLOWED_REGISTRIES` |
| Maximum command length of time | `spec.shell.maxTimeoutSeconds` |

Enforcement is always `enforce`: a connection to a host the class does not allow is denied,
and OpenShell proposes the host in the draft inbox, so a person can approve it from the UI
([egress approvals](openshell-sandbox.md#egress-approvals)). The sandbox has no model access and no
tool credentials. It has only the secrets its class's
[`credentials`](openshell-sandbox.md#class-credentials) give it, and never as values: commands
see a placeholder that the supervisor replaces on the one endpoint each credential is bound to.

## Credentials

A class with a `githubApp` credential lets commands `git clone`, `fetch` and (with
`contents: write`) `push` to its repositories, with no setup in the image: the operator
configures git to use the credential. A class with a `secretRef` credential gives commands
a placeholder in `$AGENTORCA_CRED_<NAME>` to send in the header the API expects:

```
curl -H "Authorization: Bearer $AGENTORCA_CRED_WAREHOUSE" https://api.example.com/v1/jobs
```

The credential works only on its bound host, only for the binaries and paths it names, and
only as far as the class's egress rule for that host allows (read-only unless it says
`read-write`). No sandbox is created without the credentials its class promises: a missing
Secret, a Secret outside the workload's tenant or a missing GitHub App fails the call with
the reason. A GitHub token is renewed while commands run (the shell checks on each command
and while a command is polled).
Example: [`sandboxclass-coder.yaml`](../config/samples/sandboxclass-coder.yaml). Full
reference: [class credentials](openshell-sandbox.md#class-credentials).

## How a call runs

```
model-router ──POST /agentrun/{ns}/{run}/shell──▶ operator ──▶ OpenShell gateway ──▶ sandbox
      │  (202 until the sandbox is ready)
      └─GET  .../shell/{id}?wait=20 (long poll) ──▶ polls the job's state and output
```

The router never talks to the gateway; the operator owns those credentials. Each command is a
detached job in the sandbox, so a long build does not hold a request open and survives an
operator restart. The router stops the job if the command times out or the run is cancelled.

## Lifecycle

- **Created** by the first `sandbox_exec` call of a run, named from the run, and recorded in the
  `agentorca.io/shell-sandbox` annotation on the AgentRun.
- **Deleted** when the run ends, is deleted, or is retried after a failure; the operator removes
  it by that annotation. Sandboxes also carry the labels `agentorca.io/run`,
  `agentorca.io/run-namespace` and `agentorca.io/shell`, so a leftover one can be found on the gateway.
- A run that ended cannot create a sandbox. A sandbox that fails is deleted, and the next call
  creates a new one (installed tools are lost).

## Limitations

- **One sandbox per run, not per conversation.** A warm pod of an AgentDeployment gets the
  tool too, but the sandbox belongs to the run the pod is claimed for. In a chat each message
  is a new run, so files and installed tools from an earlier message are gone.
- **Credentials only through the class.** There is no per-Agent git or secret setting for the
  shell; give the class a [credential](openshell-sandbox.md#class-credentials). A rotated
  `secretRef` Secret reaches the next run's sandbox, not a running one.
- **Not tied to tool side-effect classes.** Every command is allowed; the sandbox's policy is the
  control. A per-command approval would be a follow-up on
  [tool side effects](tool-side-effects.md).
- **Output is a tail.** Redirect large output to a file in the sandbox and read it in pieces.
