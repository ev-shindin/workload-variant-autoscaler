# feat: add V2 engine e2e tests running in parallel with V1

## Summary
- Add parallel V2 saturation engine e2e smoke tests that run automatically on every PR alongside existing V1 tests
- Add comment-triggered V2 full e2e tests via ChatOps commands with `-v2` suffix (`/trigger-e2e-full-v2`, `/test-e2e-full-v2`, `/test-full-v2`)
- Plumb `ANALYZER_NAME` env var through Makefile -> install.sh -> Helm values -> ConfigMap so V2 tests deploy with `analyzerName: saturation`
- Report V1 and V2 e2e status as separate GitHub commit status contexts

## How it works

V2 jobs are exact mirrors of V1 jobs with one difference: they set `ANALYZER_NAME: "saturation"` which flows through `install.sh` into the Helm chart, rendering `analyzerName: saturation` in the `wva-cp-configmap`. This activates the V2 capacity-constraint analyzer in the engine's dispatch logic.

```
ANALYZER_NAME env var
  -> Makefile deploy-e2e-infra
    -> install.sh
      -> helm install --set wva.capacityScaling.default.analyzerName=saturation
        -> wva-cp-configmap.yaml renders analyzerName: saturation
          -> engine.go dispatches to optimizeV2()
```

## Files changed

| File | Change |
|------|--------|
| `.github/workflows/ci-pr-checks.yaml` | 3 new jobs (`check-full-tests-v2`, `e2e-tests-smoke-v2`, `e2e-tests-full-v2`) + split `report-status` into V1/V2 |
| `.github/workflows/ci-e2e-openshift.yaml` | V2 triggers (`/ok-to-test-v2`, `/retest-v2`) + `analyzer_name` workflow input + V2 status context |
| `Makefile` | Pass `ANALYZER_NAME` to `deploy-e2e-infra` |
| `charts/.../wva-cp-configmap.yaml` | Conditionally render `analyzerName` when non-empty |
| `charts/.../values.yaml` | Add `analyzerName: ""` field with documentation |
| `charts/.../values-dev.yaml` | Add `analyzerName: ""` field with documentation |
| `deploy/install.sh` | Accept `ANALYZER_NAME` env var and pass as `--set` to Helm |

## ChatOps commands

### CI PR Checks (simulator-based)

| Command | Effect |
|---------|--------|
| `/trigger-e2e-full` | Trigger V1 full e2e tests (unchanged) |
| `/test-e2e-full` | Trigger V1 full e2e tests (unchanged) |
| `/test-full` | Trigger V1 full e2e tests (unchanged) |
| `/trigger-e2e-full-v2` | Trigger V2 full e2e tests (new) |
| `/test-e2e-full-v2` | Trigger V2 full e2e tests (new) |
| `/test-full-v2` | Trigger V2 full e2e tests (new) |

### OpenShift E2E (real GPU)

| Command | Effect |
|---------|--------|
| `/ok-to-test` | Approve and run V1 OpenShift E2E (unchanged) |
| `/retest` | Re-run V1 OpenShift E2E (unchanged) |
| `/ok-to-test-v2` | Approve and run V2 OpenShift E2E (new) |
| `/retest-v2` | Re-run V2 OpenShift E2E (new) |

## Test plan
- [ ] V1 smoke tests still pass (no regression)
- [ ] V2 smoke tests pass with `analyzerName: saturation`
- [ ] ChatOps `/test-full-v2` triggers V2 full tests correctly
- [ ] V1 ChatOps commands (`/test-e2e-full`) still work unchanged
- [ ] Report-status shows separate V1 and V2 status contexts
- [ ] Empty `ANALYZER_NAME` (default) produces V1 behavior (no `analyzerName` in ConfigMap)
- [ ] OpenShift `/ok-to-test-v2` deploys with V2 analyzer
