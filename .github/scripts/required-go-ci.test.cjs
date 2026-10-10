'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {requireGoCI} = require('./required-go-ci.cjs');
const workflowDir = path.join(__dirname, '..', 'workflows');
const ciName = fs.readFileSync(path.join(workflowDir, 'ci.yaml'), 'utf8').match(/^name: (.+)$/m)[1];
const repository = fs.readFileSync(path.join(workflowDir, 'publish-main.yaml'), 'utf8')
  .match(/expectedRepository: '(EnvPlane\/[^']+)'/)[1];
const sha = 'a'.repeat(40);

function fixture() {
  let clock = 0;
  const run = {id: 100, workflow_id: 9, path: '.github/workflows/ci.yaml',
    name: ciName, head_sha: sha, head_branch: 'main', event: 'push',
    repository: {full_name: repository}, head_repository: {full_name: repository},
    run_attempt: 1, status: 'completed', conclusion: 'success'};
  const job = {name: 'test', head_sha: sha, run_id: run.id, status: 'completed', conclusion: 'success'};
  const state = {run, job, lists: 0, sleeps: 0, outputs: [], calls: []};
  const response = data => ({status: 200, data});
  const github = {rest: {
    git: {getRef: async () => response({object: {type: 'commit', sha}})},
    repos: {compareCommits: async () => response({status: 'identical',
      base_commit: {sha}, merge_base_commit: {sha}})},
    actions: {
      getWorkflow: async () => response({id: 9, path: run.path, name: ciName, state: 'active'}),
      listWorkflowRuns: async params => {
        assert.equal(params.workflow_id, 9);
        assert.equal(params.head_sha, sha);
        assert.equal(params.branch, 'main');
        assert.equal(params.event, 'push');
        assert.equal(params.exclude_pull_requests, true);
        assert.ok(params.request.timeout <= 30000);
        state.lists++;
        return response({total_count: 1, workflow_runs: [structuredClone(run)]});
      },
      listJobsForWorkflowRunAttempt: async params => {
        assert.equal(params.attempt_number, run.run_attempt);
        assert.equal(params.run_id, run.id);
        state.calls.push('jobs');
        return response({total_count: 1, jobs: [structuredClone(job)]});
      },
      getWorkflowRun: async () => response(structuredClone(run))
    }
  }};
  const options = {github, expectedRepository: repository, expectedName: ciName,
    context: {repo: {owner: 'EnvPlane', repo: repository.split('/')[1]}, sha,
      runId: 500, eventName: 'push', ref: 'refs/heads/main', payload: {repository: {full_name: repository}}},
    core: {info() {}, setOutput(key, value) {state.outputs.push([key, value]);}},
    now: () => clock, sleep: async ms => {clock += ms; state.sleeps++;},
    maxWaitMs: 30, pollMs: 10};
  return {options, state};
}

test('exact successful main CI and current test attempt releases exact SHA', async () => {
  const {options, state} = fixture();
  await requireGoCI(options);
  assert.deepEqual(state.outputs, [['sha', sha]]);
  assert.equal(state.lists, 2);
  assert.equal(state.sleeps, 0);
});
test('manual main and main-history version tags preserve publication support', async () => {
  for (const [eventName, ref] of [['workflow_dispatch', 'refs/heads/main'], ['push', 'refs/tags/v1.2.3']]) {
    const {options} = fixture();
    Object.assign(options.context, {eventName, ref});
    await requireGoCI(options);
  }
});
test('GitHub main-qualified run path is accepted, not another ref', async () => {
  for (const suffix of ['@main', '@refs/heads/main']) {
    const {options, state} = fixture();
    state.run.path += suffix;
    options.github.rest.actions.getWorkflow = async () => ({status: 200,
      data: {id: 9, path: '.github/workflows/ci.yaml', name: ciName, state: 'active'}});
    await requireGoCI(options);
  }
});
for (const conclusion of ['failure', 'cancelled', 'timed_out', 'skipped', 'neutral', 'action_required', null]) {
  test('completed CI ' + conclusion + ' refuses publication', async () => {
    const {options, state} = fixture();
    state.run.conclusion = conclusion;
    await assert.rejects(requireGoCI(options));
    assert.equal(state.outputs.length, 0);
  });
}
for (const status of ['queued', 'in_progress', 'waiting', 'pending']) {
  test(status + ' reaches bounded deadline without publication', async () => {
    const {options, state} = fixture();
    state.run.status = status;
    state.run.conclusion = null;
    await assert.rejects(requireGoCI(options), /bounded deadline/);
    assert.equal(state.sleeps, 3);
    assert.equal(state.outputs.length, 0);
  });
}
test('missing required CI waits bounded, never passes on empty inventory', async () => {
  const {options, state} = fixture();
  options.github.rest.actions.listWorkflowRuns = async () => ({status: 200,
    data: {total_count: 0, workflow_runs: []}});
  await assert.rejects(requireGoCI(options), /bounded deadline/);
  assert.equal(state.sleeps, 3);
  assert.equal(state.outputs.length, 0);
});
test('pending CI may finish within deadline', async () => {
  const {options, state} = fixture();
  state.run.status = 'in_progress';
  options.sleep = async () => {state.run.status = 'completed';};
  await requireGoCI(options);
  assert.equal(state.outputs.length, 1);
});
for (const change of [
  r => {r.head_sha = 'b'.repeat(40);}, r => {r.head_branch = 'feature';},
  r => {r.event = 'pull_request';}, r => {r.event = 'workflow_run';},
  r => {r.workflow_id = 8;}, r => {r.path = '.github/workflows/publish-main.yaml';},
  r => {r.path += '@feature';}, r => {r.repository.full_name = 'attacker/fork';},
  r => {r.head_repository.full_name = 'attacker/fork';}, r => {r.id = 500;},
  r => {r.run_attempt = 0;}, r => {r.name = 'Publish image';}
]) {
  test('rejects mismatched/self/untrusted run: ' + change.toString(), async () => {
    const {options, state} = fixture();
    // Workflow identity remains canonical even if an untrusted run claims otherwise.
    options.github.rest.actions.getWorkflow = async () => ({status: 200,
      data: {id: 9, path: '.github/workflows/ci.yaml', name: ciName, state: 'active'}});
    change(state.run);
    await assert.rejects(requireGoCI(options));
    assert.equal(state.outputs.length, 0);
  });
}
test('newer pending run masks older success', async () => {
  const {options, state} = fixture();
  options.github.rest.actions.listWorkflowRuns = async () => ({status: 200,
    data: {total_count: 2, workflow_runs: [state.run, {...state.run, id: 101,
      status: 'queued', conclusion: null}]}});
  await assert.rejects(requireGoCI(options), /bounded deadline/);
  assert.equal(state.outputs.length, 0);
});
test('rerun started during job inspection cannot use old successful attempt', async () => {
  const {options, state} = fixture();
  const jobs = options.github.rest.actions.listJobsForWorkflowRunAttempt;
  options.github.rest.actions.listJobsForWorkflowRunAttempt = async params => {
    const result = await jobs(params);
    state.run.run_attempt = 2;
    state.run.status = 'in_progress';
    state.run.conclusion = null;
    return result;
  };
  await assert.rejects(requireGoCI(options), /bounded deadline/);
  assert.equal(state.outputs.length, 0);
});
for (const change of [
  j => {j.name = 'lint';}, j => {j.status = 'queued';}, j => {j.conclusion = 'skipped';},
  j => {j.head_sha = 'b'.repeat(40);}, j => {j.run_id = 99;}
]) {
  test('successful workflow cannot hide missing/skipped/wrong-SHA test job: ' + change.toString(), async () => {
    const {options, state} = fixture();
    change(state.job);
    await assert.rejects(requireGoCI(options), /test job/);
    assert.equal(state.outputs.length, 0);
  });
}
test('truncated inventory is not successful evidence', async () => {
  const {options} = fixture();
  options.github.rest.actions.listWorkflowRuns = async () => ({status: 200,
    data: {total_count: 101, workflow_runs: []}});
  await assert.rejects(requireGoCI(options), /inventory incomplete/);
});
for (const area of ['getRef', 'compareCommits', 'getWorkflow', 'listWorkflowRuns',
  'listJobsForWorkflowRunAttempt', 'getWorkflowRun']) {
  test('API error at ' + area + ' fails closed without leaking raw payload', async () => {
    const {options, state} = fixture();
    const group = area === 'getRef' ? 'git' : area === 'compareCommits' ? 'repos' : 'actions';
    options.github.rest[group][area] = async () => {throw new Error('sensitive payload');};
    await assert.rejects(requireGoCI(options), error =>
      error.message.includes('API unavailable') && !error.message.includes('sensitive'));
    assert.equal(state.outputs.length, 0);
  });
}
test('off-main tag, foreign repository, PR/manual feature cannot publish', async () => {
  const mutations = [
    o => {o.github.rest.repos.compareCommits = async () => ({status: 200,
      data: {status: 'diverged', base_commit: {sha}, merge_base_commit: {sha: 'b'.repeat(40)}}});},
    o => {o.context.repo.owner = 'attacker';},
    o => {o.context.eventName = 'pull_request';},
    o => {o.context.eventName = 'workflow_run';},
    o => {o.context.eventName = 'workflow_dispatch'; o.context.ref = 'refs/heads/feature';}
  ];
  for (const mutate of mutations) {
    const {options, state} = fixture();
    mutate(options);
    await assert.rejects(requireGoCI(options));
    assert.equal(state.outputs.length, 0);
  }
});
test('publisher DAG gates every existing publishing step without privileged gate credentials', () => {
  const yaml = fs.readFileSync(path.join(workflowDir, 'publish-main.yaml'), 'utf8');
  assert.ok(!/^  workflow_run:/m.test(yaml));
  const gate = yaml.split('  required-go-ci:\n')[1].split('  publish:\n')[0];
  assert.match(gate, /timeout-minutes: 25/);
  assert.match(gate, /contents: read\n      actions: read/);
  assert.match(gate, /ref: refs\/heads\/main/);
  assert.match(gate, /persist-credentials: false/);
  assert.match(gate, /retries: 0/);
  assert.match(gate, /actions\/github-script@ed597411d8f924073f98dfc5c65a23a2325f34cd/);
  assert.ok(!gate.includes('secrets.') && !gate.includes(': write'));
  assert.match(yaml, /  publish:\n    needs: required-go-ci\n    if: github.repository ==/);
  assert.match(yaml, /push: true/);
  assert.ok(!/needs: required-go-ci\n    if:.*always\(/.test(yaml));
  assert.ok(fs.readFileSync(path.join(__dirname, 'required-go-ci.cjs'), 'utf8').includes('ci.yaml'));
  assert.match(fs.readFileSync(path.join(workflowDir, 'ci.yaml'), 'utf8'),
    /run: node --test \.github\/scripts\/required-go-ci\.test\.cjs/);
});
