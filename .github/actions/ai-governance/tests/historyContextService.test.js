const baseConfig = require('../config.json');
const { applyLocale } = require('../src/utils/config');
const HistoryContextService = require('../src/services/historyContextService');
const githubOps = require('../src/services/github');

function buildConfig() {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  return config;
}

// search API 原始 item 形状（与 githubOps.test.js 契约一致）
function rawItem(overrides = {}) {
  return {
    number: 10,
    title: '某个历史条目',
    body: '正文',
    state: 'closed',
    state_reason: 'not_planned',
    closed_at: '2026-01-02T00:00:00Z',
    labels: [{ name: 'bug' }],
    ...overrides
  };
}

function makeOps({
  searchItems = [],
  canonicalItems = [],
  issueDetail = null,
  comments = [],
  timeline = [],
  commits = [],
  files = { data: [] }
} = {}) {
  return {
    searchIssuesAndPRs: jest.fn().mockResolvedValue(searchItems),
    listCanonicalIssues: jest.fn().mockResolvedValue(canonicalItems),
    getIssueDetail: jest.fn().mockImplementation(async (_o, _r, _repo, number) =>
      issueDetail || { number, title: `标题${number}`, body: '正文', state: 'closed', state_reason: 'not_planned', closed_at: '2026-01-02T00:00:00Z' }
    ),
    listIssueComments: jest.fn().mockResolvedValue(comments),
    listIssueTimeline: jest.fn().mockResolvedValue(timeline),
    listPRCommits: jest.fn().mockResolvedValue(commits),
    listPRFilesSummary: jest.fn().mockResolvedValue(files)
  };
}

function makeService(gov = {}, ops) {
  const config = buildConfig();
  return new HistoryContextService({}, config, gov, ops);
}

describe('HistoryContextService.buildIndex', () => {
  test('混合语料：issue 与 PR 并存，PR 合并态映射为 merged，含 state/labels', async () => {
    const ops = makeOps({
      searchItems: [
        { number: 10, kind: 'issue', title: '历史 issue', labels: ['bug'], state: 'closed', state_reason: 'not_planned', closed_at: '2026-01-02T00:00:00Z' },
        { number: 30, kind: 'pr', title: '已合并 PR', labels: [], state: 'merged', state_reason: null, closed_at: '2026-03-01T00:00:00Z' },
        { number: 31, kind: 'pr', title: '开放 PR', labels: [], state: 'open', state_reason: null, closed_at: null }
      ]
    });
    const svc = makeService({}, ops);

    const index = await svc.buildIndex('o', 'r');

    expect(index).toHaveLength(3);
    expect(index.find(i => i.number === 10)).toMatchObject({ kind: 'issue', state: 'closed' });
    expect(index.find(i => i.number === 30)).toMatchObject({ kind: 'pr', state: 'merged' });
    expect(index.find(i => i.number === 31)).toMatchObject({ kind: 'pr', state: 'open' });
  });

  test('去重：search 与 canonical 命中同一编号只保留一份（canonical 优先，带 canonical 标记）', async () => {
    const ops = makeOps({
      searchItems: [
        { number: 57, kind: 'issue', title: '缓存', labels: ['enhancement'], state: 'closed', state_reason: 'completed', closed_at: null }
      ],
      canonicalItems: [
        { number: 57, title: '缓存 canonical', body: 'x', state: 'closed', state_reason: 'not_planned', closed_at: '2026-01-02T00:00:00Z' }
      ]
    });
    const svc = makeService({}, ops);

    const index = await svc.buildIndex('o', 'r');

    const hit = index.filter(i => i.number === 57);
    expect(hit).toHaveLength(1);
    expect(hit[0].labels).toContain('canonical');
    expect(hit[0].title).toBe('缓存 canonical');
  });

  test('canonical 保证入选：即使 search 未命中也进入索引', async () => {
    const ops = makeOps({
      searchItems: [],
      canonicalItems: [
        { number: 88, title: '只出现在 canonical 的结论', body: 'x', state: 'closed', state_reason: 'not_planned', closed_at: null }
      ]
    });
    const svc = makeService({}, ops);

    const index = await svc.buildIndex('o', 'r');

    expect(index.find(i => i.number === 88)).toMatchObject({ kind: 'issue', labels: ['canonical'] });
  });

  test('上限截断：不超过 maxHistoryIndex', async () => {
    const ops = makeOps({
      searchItems: Array.from({ length: 150 }, (_, i) => ({
        number: i + 1, kind: 'issue', title: `t${i}`, labels: [], state: 'closed', state_reason: null, closed_at: null
      }))
    });
    const svc = makeService({ maxHistoryIndex: 40 }, ops);

    const index = await svc.buildIndex('o', 'r');

    expect(index).toHaveLength(40);
  });

  test('search 失败：回落 canonical-only 索引（fail-soft，不抛错）', async () => {
    const ops = makeOps({ canonicalItems: [{ number: 57, title: 'c', body: 'x', state: 'closed', state_reason: null, closed_at: null }] });
    ops.searchIssuesAndPRs.mockRejectedValue(new Error('search down'));
    const svc = makeService({}, ops);

    const index = await svc.buildIndex('o', 'r');

    expect(index).toHaveLength(1);
    expect(index[0].number).toBe(57);
  });

  test('canonical 检索失败：回落 search-only（fail-soft）', async () => {
    const ops = makeOps({
      searchItems: [{ number: 10, kind: 'issue', title: 's', labels: [], state: 'closed', state_reason: null, closed_at: null }]
    });
    ops.listCanonicalIssues.mockRejectedValue(new Error('canonical down'));
    const svc = makeService({}, ops);

    const index = await svc.buildIndex('o', 'r');

    expect(index).toHaveLength(1);
    expect(index[0].number).toBe(10);
  });

  test('双通道全失败：返回 []（fail-soft，不抛错）', async () => {
    const ops = makeOps({});
    ops.searchIssuesAndPRs.mockRejectedValue(new Error('down'));
    ops.listCanonicalIssues.mockRejectedValue(new Error('down'));
    const svc = makeService({}, ops);

    await expect(svc.buildIndex('o', 'r')).resolves.toEqual([]);
  });

  test('契约（R2/C17）：listCanonicalIssues 真实输出形状（含 FIX-B 的 state 字段）直通 buildIndex', async () => {
    // 直接调用真实 ops 的 listCanonicalIssues 拿到真实映射形状，喂给 buildIndex 的合并通道
    const octokit = {
      rest: {
        search: {
          issuesAndPullRequests: jest.fn().mockResolvedValue({
            data: { items: [rawItem({ number: 57, labels: [{ name: 'canonical' }] })] }
          })
        }
      }
    };
    const real = await githubOps.listCanonicalIssues(octokit, 'o', 'r', 'canonical', 50, 1500, true);

    const ops = makeOps({ canonicalItems: real, searchItems: [] });
    const svc = makeService({}, ops);

    const index = await svc.buildIndex('o', 'r');

    // 真实形状经 buildIndex 后仍携带全部下游需要的字段
    expect(index).toEqual([{
      number: 57,
      kind: 'issue',
      title: '某个历史条目',
      labels: ['canonical'],
      state: 'closed',
      state_reason: 'not_planned',
      closed_at: '2026-01-02T00:00:00Z',
      body: '正文'
    }]);
  });
});

describe('HistoryContextService.enrich', () => {
  test('issue 补全：body 截断 + 评论截断 500 + 时间线白名单过滤', async () => {
    const ops = makeOps({
      issueDetail: { number: 57, title: '缓存', body: '长'.repeat(3000), state: 'closed', state_reason: 'not_planned', closed_at: '2026-01-02T00:00:00Z' },
      comments: [
        { author: 'maintainer', body: '不做了，wontfix', created_at: '2026-01-01' },
        { author: 'x', body: 'b'.repeat(800), created_at: '2026-01-02' }
      ],
      timeline: [
        { event: 'closed', actor: 'maintainer', commit_id: null, created_at: '2026-01-02' },
        { event: 'subscribed', actor: 'x', commit_id: null, created_at: '2026-01-03' } // 噪音，应被过滤
      ]
    });
    const svc = makeService({}, ops);

    const details = await svc.enrich('o', 'r', [{ number: 57, kind: 'issue' }]);

    expect(details).toHaveLength(1);
    const d = details[0];
    expect(d.kind).toBe('issue');
    expect(d.body.length).toBeLessThanOrEqual(1505);
    expect(d.body).toMatch(/…\(截断\)$/);
    expect(d.comments[0]).toEqual({ author: 'maintainer', body: '不做了，wontfix', created_at: '2026-01-01' });
    expect(d.comments[1].body.length).toBeLessThanOrEqual(505);
    expect(d.timeline).toEqual([{ event: 'closed', actor: 'maintainer', commit_id: null }]);
  });

  test('PR 补全：额外含 files_changed 摘要与提交列表', async () => {
    const ops = makeOps({
      comments: [{ author: 'a', body: 'c', created_at: '2026-01-01' }],
      timeline: [],
      commits: [{ message: 'feat: add cache', author: 'someone' }],
      files: { summary: 'src/a.js(+10/-2)', total: 1 }
    });
    const svc = makeService({}, ops);

    const details = await svc.enrich('o', 'r', [{ number: 30, kind: 'pr' }]);

    expect(details).toHaveLength(1);
    expect(details[0].files_changed).toBe('src/a.js(+10/-2)');
    expect(details[0].commits).toEqual([{ message: 'feat: add cache', author: 'someone' }]);
    expect(ops.listPRFilesSummary).toHaveBeenCalledWith({}, 'o', 'r', 30, expect.anything());
    expect(ops.listPRCommits).toHaveBeenCalledWith({}, 'o', 'r', 30, 20);
  });

  test('单条失败跳过：其余条目照常返回（容忍语义）', async () => {
    const ops = makeOps({});
    ops.getIssueDetail.mockImplementation(async (_o, _r, _repo, number) => {
      if (number === 57) {
        throw new Error('boom');
      }
      return { number, title: 'ok', body: 'b', state: 'closed', state_reason: null, closed_at: null };
    });
    const svc = makeService({}, ops);

    const details = await svc.enrich('o', 'r', [
      { number: 57, kind: 'issue' },
      { number: 63, kind: 'issue' }
    ]);

    expect(details).toHaveLength(1);
    expect(details[0].number).toBe(63);
  });

  test('全部失败：返回 []（调用方回落）', async () => {
    const ops = makeOps({});
    ops.getIssueDetail.mockRejectedValue(new Error('all down'));
    const svc = makeService({}, ops);

    const details = await svc.enrich('o', 'r', [{ number: 57, kind: 'issue' }, { number: 58, kind: 'issue' }]);

    expect(details).toEqual([]);
  });
});
