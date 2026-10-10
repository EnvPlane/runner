'use strict';

// Read-only publisher prerequisite. Never trust check names, PR runs, artifacts,
// dispatch inputs, or a successful older run when a newer run/rerun exists.
const WORKFLOW = '.github/workflows/ci.yaml';
const MAX_WAIT_MS = 20 * 60 * 1000;

async function requireGoCI({github, context, core, expectedRepository, expectedName,
  now = Date.now, sleep = ms => new Promise(resolve => setTimeout(resolve, ms)),
  maxWaitMs = MAX_WAIT_MS, pollMs = 15000}) {
  const repoName = context.repo.owner + '/' + context.repo.repo;
  const sha = context.sha;
  if (repoName !== expectedRepository || context.payload.repository?.full_name !== repoName ||
      !/^[0-9a-f]{40}$/.test(sha) ||
      !((context.eventName === 'push' &&
         (context.ref === 'refs/heads/main' || /^refs\/tags\/v[^/]+$/.test(context.ref))) ||
        (context.eventName === 'workflow_dispatch' && context.ref === 'refs/heads/main'))) {
    throw new Error('Publication requires a trusted repository main SHA or main-history version tag');
  }
  if (!(maxWaitMs > 0 && maxWaitMs <= MAX_WAIT_MS && pollMs > 0)) {
    throw new Error('Invalid bounded CI wait');
  }
  const deadline = now() + maxWaitMs;
  const args = {...context.repo, request: {timeout: 30000}};
  const api = async (fn, params) => {
    if (now() >= deadline) throw new Error('Required Go CI bounded deadline exceeded');
    try {
      const result = await fn({...args, ...params,
        request: {timeout: Math.min(30000, deadline - now())}});
      if (result.status !== 200 || !result.data) throw new Error('Invalid response');
      return result.data;
    } catch {
      // Never log raw API errors, authorization headers, or response payloads.
      throw new Error('Required Go CI API unavailable; publication refused');
    }
  };
  const main = await api(github.rest.git.getRef, {ref: 'heads/main'});
  if (!/^[0-9a-f]{40}$/.test(main.object?.sha) || main.object?.type !== 'commit') {
    throw new Error('Trusted main reference unavailable');
  }
  const ancestry = await api(github.rest.repos.compareCommits, {base: sha, head: main.object.sha});
  if (!['ahead', 'identical'].includes(ancestry.status) ||
      ancestry.base_commit?.sha !== sha || ancestry.merge_base_commit?.sha !== sha) {
    throw new Error('Publication SHA is not in trusted main history');
  }
  const workflow = await api(github.rest.actions.getWorkflow, {workflow_id: 'ci.yaml'});
  if (!Number.isSafeInteger(workflow.id) || workflow.id <= 0 ||
      workflow.path !== WORKFLOW || workflow.name !== expectedName || workflow.state !== 'active') {
    throw new Error('Required Go CI workflow identity invalid');
  }
  const runs = async () => {
    const data = await api(github.rest.actions.listWorkflowRuns, {
      workflow_id: workflow.id, head_sha: sha, branch: 'main', event: 'push',
      exclude_pull_requests: true, per_page: 100
    });
    if (!Number.isSafeInteger(data.total_count) || data.total_count < 0 ||
        data.total_count > 100 || !Array.isArray(data.workflow_runs) ||
        data.workflow_runs.length !== data.total_count) {
      throw new Error('Required Go CI run inventory incomplete');
    }
    for (const run of data.workflow_runs) {
      validateRun(run);
    }
    return data.workflow_runs.sort((a, b) => b.id - a.id)[0];
  };
  function validateRun(run) {
    if (!Number.isSafeInteger(run.id) || run.id <= 0 || run.id === Number(context.runId) ||
        run.workflow_id !== workflow.id ||
        ![WORKFLOW, WORKFLOW + '@main', WORKFLOW + '@refs/heads/main'].includes(run.path) ||
        run.name !== expectedName ||
        run.head_sha !== sha || run.head_branch !== 'main' || run.event !== 'push' ||
        run.repository?.full_name !== repoName || run.head_repository?.full_name !== repoName ||
        !Number.isSafeInteger(run.run_attempt) || run.run_attempt < 1) {
      throw new Error('Required Go CI run is not exact trusted main evidence');
    }
  }
  while (now() < deadline) {
    const run = await runs();
    if (run?.status === 'completed') {
      if (run.conclusion !== 'success') throw new Error('Required Go CI failed; publication refused');
      const jobs = await api(github.rest.actions.listJobsForWorkflowRunAttempt, {
        run_id: run.id, attempt_number: run.run_attempt, per_page: 100
      });
      if (!Number.isSafeInteger(jobs.total_count) || jobs.total_count > 100 ||
          !Array.isArray(jobs.jobs) || jobs.total_count !== jobs.jobs.length) {
        throw new Error('Required Go CI job inventory incomplete');
      }
      const required = jobs.jobs.filter(job => job.name === 'test');
      if (required.length !== 1 || required[0].head_sha !== sha ||
          required[0].run_id !== run.id || required[0].status !== 'completed' ||
          required[0].conclusion !== 'success') {
        throw new Error('Required Go CI test job did not succeed for this SHA');
      }
      // Catch a rerun/new run started during job inspection. Only the current
      // latest run and current attempt may release the publisher.
      const fresh = await api(github.rest.actions.getWorkflowRun, {run_id: run.id});
      validateRun(fresh);
      const latest = await runs();
      if (fresh.status === 'completed' && fresh.conclusion === 'success' &&
          fresh.run_attempt === run.run_attempt && latest?.id === run.id &&
          latest.run_attempt === run.run_attempt && latest.status === 'completed' &&
          latest.conclusion === 'success' && now() < deadline) {
        core.info('Required Go CI succeeded for exact main SHA ' + sha + ' (run ' + run.id + ')');
        core.setOutput('sha', sha);
        return;
      }
    }
    if (now() >= deadline) break;
    core.info('Waiting for required Go CI on exact publication SHA');
    await sleep(Math.min(pollMs, deadline - now()));
  }
  throw new Error('Required Go CI missing or pending at bounded deadline; publication refused');
}

module.exports = {requireGoCI};
