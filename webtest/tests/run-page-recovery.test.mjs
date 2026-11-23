import test from 'node:test';
import assert from 'node:assert/strict';
import { chooseOption, loadApp, settle } from '../lib/load-app.mjs';

async function traceFixture(t) {
  const app = await loadApp();
  t.after(app.close);
  const run = {
    ...app.evaluate('structuredClone(state.data.runs[0])'),
    status: 'running',
    revision: 1,
    telemetrySequence: 2,
    logSequence: 0,
    workflowSnapshot: app.evaluate('structuredClone(state.data.workflows[0])'),
    nodes: { 'node-1': { status: 'running' }, 'node-2': { status: 'pending' } },
  };
  const events = [];
  const afters = [];
  const fixture = {
    app, run, afters,
    detail: run,
    makeEvents(total) {
      for (let sequence = events.length + 1; sequence <= total; sequence++) {
        events.push({
          id: `trace-${sequence}`, sequence, nodeId: sequence % 4 === 0 ? 'node-1' : 'node-2',
          category: 'verification', name: 'step', message: `Trace event ${sequence}`,
          timestamp: '2026-10-21T10:02:00Z',
        });
      }
    },
    emit(status, revision) {
      this.stream.emit({
        run: { ...run, status, revision, telemetrySequence: events.length },
        telemetryDelta: events.slice(-80), logDelta: [],
      }, 'run');
    },
    rendered() {
      return [...app.document.querySelectorAll('.telemetry-feed article p')].map(item => item.textContent);
    },
  };
  fixture.makeEvents(2);
  app.router.get('/api/runs/run-1', () => fixture.detail);
  app.router.get(/^\/api\/runs\/run-1\/logs\?/, { logs: [], sequence: 0, hasMore: false });
  app.router.get(/^\/api\/runs\/run-1\/telemetry\?/, ({ url }) => {
    const after = url.searchParams.get('after');
    const node = url.searchParams.get('nodeId');
    const filtered = events.filter(e => !node || e.nodeId === node);
    const available = after === null ? filtered : filtered.filter(e => e.sequence > Number(after));
    const page = after === null ? available.slice(-100) : available.slice(0, 100);
    const body = {
      events: page, hasMore: available.length > page.length, sequence: events.length,
      nextBefore: page[0]?.sequence, nextAfter: page.at(-1)?.sequence,
    };
    if (after === null) return body;
    afters.push(Number(after));
    return fixture.delay ? fixture.delay(body) : body;
  });
  app.evaluate(`state.data.runs=[${JSON.stringify(run)}];switchView('runs');showRunDetail('run-1')`);
  await settle(7);
  app.document.querySelector('[data-run-tab="trace"]').click();
  fixture.stream = app.evaluate('state.runStream');
  assert.deepEqual(fixture.rendered(), ['Trace event 2', 'Trace event 1']);
  return fixture;
}

test('the live trace drains missed pages once before closing on the terminal revision', async (t) => {
  const f = await traceFixture(t);
  let release;
  f.delay = body => new Promise(resolve => { release = () => { f.delay = null; resolve(body); }; });
  f.makeEvents(232);
  f.emit('running', 2);
  f.emit('succeeded', 3);
  await settle(5);
  assert.deepEqual(f.afters, [2], 'one catch-up fetch starts at the last covered sequence');
  assert.equal(f.stream.readyState, 1, 'the stream stays open during catch-up');
  release();
  await settle(10);
  assert.deepEqual(f.afters, [2, 102, 202]);
  assert.deepEqual(f.rendered(), Array.from({ length: 232 }, (_, i) => `Trace event ${232 - i}`));
  assert.equal(f.stream.readyState, 2);
});

test('the polling fallback reads beyond one page before stopping at a terminal run', async (t) => {
  const f = await traceFixture(t);
  f.makeEvents(132);
  f.detail = { ...f.run, status: 'succeeded', telemetrySequence: 132, revision: 2 };
  f.stream.emit('', 'error');
  await settle(10);
  assert.deepEqual(f.afters, [2, 102]);
  assert.deepEqual(f.rendered(), Array.from({ length: 132 }, (_, i) => `Trace event ${132 - i}`));
  assert.equal(f.app.evaluate('state.poll'), null);
});

test('a terminal trace request cannot repaint a different filter or stop a different run', async (t) => {
  for (const change of ['filter', 'run']) {
    await t.test(change, async (t) => {
      const f = await traceFixture(t);
      let release;
      f.delay = body => new Promise(resolve => { release = () => resolve(body); });
      f.makeEvents(132);
      f.emit('succeeded', 2);
      await settle(4);
      assert.deepEqual(f.afters, [2]);
      if (change === 'filter') {
        chooseOption(f.app.document.querySelector('[data-run-trace-node]'), 'node-1');
      } else {
        const next = { ...f.run, id: 'run-next', telemetrySequence: 1 };
        f.app.router.get('/api/runs/run-next', next);
        f.app.router.get(/^\/api\/runs\/run-next\/logs\?/, { logs: [], sequence: 0, hasMore: false });
        f.app.router.get(/^\/api\/runs\/run-next\/telemetry\?/, {
          events: [{ id: 'next', sequence: 1, name: 'next', message: 'Next run', category: 'verification' }],
          sequence: 1, hasMore: false,
        });
        f.app.evaluate(`state.data.runs.push(${JSON.stringify(next)});showRunDetail('run-next')`);
      }
      await settle(7);
      release();
      await settle(8);
      assert.deepEqual(f.afters, [2], 'the previous filter does not request another page');
      if (change === 'filter') {
        assert.deepEqual(f.rendered(), Array.from({ length: 33 }, (_, i) => `Trace event ${132 - 4 * i}`));
        assert.equal(f.stream.readyState, 2);
      } else {
        assert.deepEqual(f.rendered(), ['Next run']);
        assert.equal(f.app.evaluate('state.runStream.readyState'), 1);
      }
    });
  }
});
