// github.js ops 层契约测试（C2/C17/R2）：mock octokit 返回 search API 的真实原始形状，
// 断言 listCanonicalIssues 映射后的返回形状 —— 下游（prReviewService canonical 通道等）
// 依赖的每个字段都必须真实存在，防止「mock 比真实实现更丰富」。

// search API 返回的原始 item 形状（GitHub REST 文档口径）
function rawSearchItem(overrides = {}) {
  return {
    number: 57,
    title: '支持缓存',
    body: '加一层缓存的诉求',
    state: 'closed',
    state_reason: 'not_planned',
    closed_at: '2026-01-02T00:00:00Z',
    labels: [{ name: 'canonical' }],
    pull_request: undefined,
    ...overrides
  };
}

function makeOctokit(items) {
  return {
    rest: {
      search: {
        issuesAndPullRequests: jest.fn().mockResolvedValue({ data: { items } })
      },
      issues: {
        listComments: jest.fn().mockResolvedValue({ data: [] }),
        listEvents: jest.fn().mockResolvedValue({ data: [] })
      },
      pulls: {
        listCommits: jest.fn().mockResolvedValue({ data: [] })
      }
    }
  };
}

const githubOps = require('../src/services/github');

describe('github ops contract', () => {
  test('listCanonicalIssues 返回 state/state_reason/closed_at（C2：canonical 通道不再饿死）', async () => {
    const octokit = makeOctokit([
      rawSearchItem(),
      rawSearchItem({ number: 63, state: 'open', state_reason: null, closed_at: null })
    ]);

    const result = await githubOps.listCanonicalIssues(octokit, 'o', 'r', 'canonical', 50, 1500, true);

    expect(result).toEqual([
      {
        number: 57,
        title: '支持缓存',
        body: '加一层缓存的诉求',
        state: 'closed',
        state_reason: 'not_planned',
        closed_at: '2026-01-02T00:00:00Z'
      },
      {
        number: 63,
        title: '支持缓存',
        body: '加一层缓存的诉求',
        state: 'open',
        state_reason: null,
        closed_at: null
      }
    ]);
  });

  test('listCanonicalIssues 查询串含 is:issue 与 label 过滤，截断保持', async () => {
    const octokit = makeOctokit([rawSearchItem({ body: 'x'.repeat(3000) })]);

    const result = await githubOps.listCanonicalIssues(octokit, 'o', 'r', 'canonical', 10, 100, true);

    const q = octokit.rest.search.issuesAndPullRequests.mock.calls[0][0].q;
    expect(q).toBe('repo:o/r is:issue label:"canonical"');
    expect(result[0].body.length).toBeLessThanOrEqual(100);
  });

  test('searchRelatedClosedIssues 返回含 is_pr 甄别字段', async () => {
    const octokit = makeOctokit([
      rawSearchItem({ number: 12 }),
      rawSearchItem({ number: 30, pull_request: { merged_at: '2026-03-01T00:00:00Z' } })
    ]);

    const result = await githubOps.searchRelatedClosedIssues(octokit, 'o', 'r', ['缓存'], 10);

    expect(result[0]).toMatchObject({ number: 12, state: 'closed', is_pr: false });
    expect(result[1]).toMatchObject({ number: 30, is_pr: true });
  });
});
