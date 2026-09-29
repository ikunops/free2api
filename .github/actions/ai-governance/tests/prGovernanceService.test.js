const baseConfig = require('../config.json');
const { applyLocale } = require('../src/utils/config');
const PrGovernanceService = require('../src/services/prGovernanceService');
const { PR_LINK_ANCHOR } = PrGovernanceService;
const IssueGovernanceService = require('../src/services/issueGovernanceService');
const { GOVERNANCE_DECISIONS } = require('../src/utils/constants');

function buildConfig() {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  return config;
}

// 构造 openai stub：按目的（要点提炼 / 归并匹配 / 标题规范化 / 起草 canonical）顺序消费
function makeOpenai(resultsByCall = []) {
  const create = jest.fn();
  resultsByCall.forEach(res => create.mockResolvedValueOnce({
    choices: [{ message: { content: res } }]
  }));
  return {
    chat: { completions: { create } },
    _create: create
  };
}

// 构造 github ops mock
function makeOps({ canonicalItems = [] } = {}) {
  return {
    listCanonicalIssues: jest.fn().mockResolvedValue(canonicalItems),
    createIssue: jest.fn().mockResolvedValue({ data: { number: 88, html_url: 'https://github.com/o/r/issues/88' } }),
    addComment: jest.fn().mockResolvedValue({}),
    addLabels: jest.fn().mockResolvedValue({}),
    updateIssueState: jest.fn().mockResolvedValue({}),
    updatePullRequest: jest.fn().mockResolvedValue({}),
    searchIssuesAndPRs: jest.fn().mockResolvedValue([]),
    getIssueDetail: jest.fn().mockImplementation(async (_o, _r, _repo, number) =>
      ({ number, title: `详情${number}`, body: '正文全文', state: 'closed', state_reason: 'completed', closed_at: null })),
    listIssueComments: jest.fn().mockResolvedValue([]),
    listIssueTimeline: jest.fn().mockResolvedValue([]),
    listPRFilesSummary: jest.fn().mockResolvedValue({ summary: 'a.js(+1/-1)', total: 1 }),
    listPRCommits: jest.fn().mockResolvedValue([])
  };
}

const pr = {
  number: 42,
  title: '加个缓存功能',
  body: '希望网关加一层缓存，缓解后端压力。',
  user: { login: 'someone' }
};

describe('PrGovernanceService', () => {
  test('匹配成功：规范化标题 + 评论关联 + 正文顶部追加 Related to + canonical 记录，绝不关闭 PR', async () => {
    const config = buildConfig();
    // 调用顺序：extract(structured) -> merge_match -> title（标题「加个缓存功能」不规范）
    const openai = makeOpenai([
      '```json\n{"要点":"加缓存","要做的事":["实现缓存"]}\n```',
      'DUPLICATE(#57)',
      'feat: add caching layer'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '缓存', body: '...' }] });
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.govern({}, 'o', 'r', pr, 'enhancement');

    expect(result.decision).toBe(GOVERNANCE_DECISIONS.DUPLICATE);
    expect(result.canonicalNumber).toBe(57);
    expect(result.linked).toBe(true);

    // 标题改写
    expect(ops.updatePullRequest).toHaveBeenCalledWith({}, 'o', 'r', 42, { title: 'feat: add caching layer' });

    // 正文顶部追加（含幂等锚点 + Related to #57）
    const bodyCall = ops.updatePullRequest.mock.calls.find(c => c[4] && c[4].body);
    expect(bodyCall).toBeTruthy();
    expect(bodyCall[4].body).toContain(PR_LINK_ANCHOR);
    expect(bodyCall[4].body).toContain('Related to #57');
    expect(bodyCall[4].body).not.toContain('Closes #57');

    // 评论（要点 + 关联说明）
    const prComment = ops.addComment.mock.calls.find(c => c[3] === 42);
    expect(prComment).toBeTruthy();
    expect(prComment[4]).toContain('#57');

    // canonical 上追加关联记录
    const recordCall = ops.addComment.mock.calls.find(c => c[3] === 57);
    expect(recordCall).toBeTruthy();
    expect(recordCall[4]).toContain('#42');
    expect(recordCall[4]).toContain('@someone');

    // 绝不通过治理关闭 PR：无 updateIssueState 调用，无 pulls.update state=closed
    expect(ops.updateIssueState).not.toHaveBeenCalled();
    const stateClose = ops.updatePullRequest.mock.calls.find(c => c[4] && c[4].state === 'closed');
    expect(stateClose).toBeUndefined();
  });

  test('匹配成功但标题已规范：不改标题，仅追加正文 + 评论', async () => {
    const config = buildConfig();
    const titled = { ...pr, title: 'feat: add caching layer' };
    // extract -> merge_match；标题已规范，不应再调 title 起草
    const openai = makeOpenai([
      '```json\n{"要点":"加缓存","要做的事":[]}\n```',
      'DUPLICATE(#57)'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '缓存', body: '...' }] });
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: false }, ops);

    await svc.govern({}, 'o', 'r', titled, null);

    // 无 title 更新
    const titleCall = ops.updatePullRequest.mock.calls.find(c => c[4] && c[4].title);
    expect(titleCall).toBeUndefined();
    // 但正文仍追加
    const bodyCall = ops.updatePullRequest.mock.calls.find(c => c[4] && c[4].body);
    expect(bodyCall).toBeTruthy();
  });

  test('新主题：创建 canonical（打 canonical+分类标签）+ 正文 Closes #N 关联块', async () => {
    const config = buildConfig();
    // extract -> merge_match(NEW_TOPIC) -> draft canonical -> title
    const openai = makeOpenai([
      '```json\n{"要点":"新能力 zzz","要做的事":["实现 zzz"]}\n```',
      'NEW_TOPIC',
      '[Feature] 支持 zzz\n\n## 概述\n支持 zzz。',
      'feat: support zzz'
    ]);
    const ops = makeOps({ canonicalItems: [] });
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.govern({}, 'o', 'r', pr, 'enhancement');

    expect(result.decision).toBe(GOVERNANCE_DECISIONS.NEW_TOPIC);
    expect(result.created).toBe(true);
    expect(result.canonicalNumber).toBe(88);

    // 创建 canonical 带 canonical + 分类标签（签名 createIssue(octokit, owner, repo, title, body, labels)）
    expect(ops.createIssue).toHaveBeenCalled();
    const createArgs = ops.createIssue.mock.calls[0];
    expect(createArgs[3]).toMatch(/^\[Feature\]/);
    expect(createArgs[5]).toEqual(['canonical', 'enhancement']);

    // 正文关联块用 Closes #88 语义
    const bodyCall = ops.updatePullRequest.mock.calls.find(c => c[4] && c[4].body);
    expect(bodyCall[4].body).toContain('Closes #88');
    expect(bodyCall[4].body).toContain(PR_LINK_ANCHOR);

    // 不关闭 PR
    expect(ops.updateIssueState).not.toHaveBeenCalled();
    expect(ops.updatePullRequest.mock.calls.find(c => c[4] && c[4].state === 'closed')).toBeUndefined();
  });

  test('幂等：正文已含锚点时跳过追加，不重复更新', async () => {
    const config = buildConfig();
    const alreadyLinked = { ...pr, body: `${PR_LINK_ANCHOR}\n\n已有内容` };
    const openai = makeOpenai([
      '```json\n{"要点":"加缓存","要做的事":[]}\n```',
      'DUPLICATE(#57)'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: 'x', body: 'x' }] });
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: false }, ops);

    await svc.govern({}, 'o', 'r', alreadyLinked, null);

    // 无任何 body 更新
    expect(ops.updatePullRequest.mock.calls.find(c => c[4] && c[4].body)).toBeUndefined();
  });

  test('dry-run：只评论不改写标题/正文、不创建 issue（匹配路径）', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"加缓存","要做的事":[]}\n```',
      'DUPLICATE(#57)'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: 'x', body: 'x' }] });
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: true }, ops);

    const result = await svc.govern({}, 'o', 'r', pr, null);

    expect(result.dryRun).toBe(true);
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
    expect(ops.createIssue).not.toHaveBeenCalled();
    expect(ops.updateIssueState).not.toHaveBeenCalled();
    expect(ops.addComment).toHaveBeenCalled();
  });

  test('dry-run：新主题路径不创建 issue', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"新能力","要做的事":[]}\n```',
      'NEW_TOPIC'
    ]);
    const ops = makeOps({ canonicalItems: [] });
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: true }, ops);

    const result = await svc.govern({}, 'o', 'r', pr, null);

    expect(result.decision).toBe(GOVERNANCE_DECISIONS.NEW_TOPIC);
    expect(result.dryRun).toBe(true);
    expect(ops.createIssue).not.toHaveBeenCalled();
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
  });

  test('AI 失败（要点提炼抛错）：向上抛出，由 handler 兜底放行，不做任何写操作', async () => {
    const config = buildConfig();
    const openai = makeOpenai([new Error('AI 挂了')]);
    openai._create.mockReset();
    openai._create.mockRejectedValueOnce(new Error('AI 挂了'));
    const ops = makeOps({ canonicalItems: [] });
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: false }, ops);

    await expect(svc.govern({}, 'o', 'r', pr, null)).rejects.toThrow('AI 挂了');
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
    expect(ops.createIssue).not.toHaveBeenCalled();
    expect(ops.updateIssueState).not.toHaveBeenCalled();
  });

  test('标题起草 AI 失败：回退原标题，仍完成正文关联（不阻断治理）', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"加缓存","要做的事":[]}\n```',
      'DUPLICATE(#57)'
    ]);
    // resolveTitle 对「加个缓存功能」会再发起一次 title 调用，让它失败
    openai._create.mockImplementationOnce(() => {
      return Promise.reject(new Error('title boom'));
    });
    const ops = makeOps({ canonicalItems: [{ number: 57, title: 'x', body: 'x' }] });
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: false }, ops);

    await svc.govern({}, 'o', 'r', pr, null);

    // 无 title 更新（回退原标题）
    expect(ops.updatePullRequest.mock.calls.find(c => c[4] && c[4].title)).toBeUndefined();
    // 正文仍完成了关联
    expect(ops.updatePullRequest.mock.calls.find(c => c[4] && c[4].body)).toBeTruthy();
  });

  test('UNCERTAIN 放行：仅评论，不建 canonical、不改标题/正文、不关闭 PR', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"模糊","要做的事":[]}\n```',
      'UNCERTAIN'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 1, title: 'x', body: 'x' }] });
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.govern({}, 'o', 'r', pr, null);

    expect(result.decision).toBe(GOVERNANCE_DECISIONS.UNCERTAIN);
    expect(result.passed).toBe(true);
    expect(ops.createIssue).not.toHaveBeenCalled();
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
    expect(ops.updateIssueState).not.toHaveBeenCalled();
    expect(ops.addComment).toHaveBeenCalled();
  });

  test('复用而非复制：PrGovernanceService 内部持有 IssueGovernanceService 实例做提炼/匹配/起草', () => {
    const config = buildConfig();
    const svc = new PrGovernanceService({}, 'model', config, {}, makeOps());
    expect(svc.issueGov).toBeInstanceOf(IssueGovernanceService);
  });

  // ---- F2/F3：两段式归并匹配（共享历史索引，C5/R5 单次拉取）----

  test('两段式开启 + ctx：归并匹配经筛选 AI，阶段二只看深补全候选（R9：含评论历史），不重复拉取', async () => {
    const config = buildConfig();
    // 调用顺序：extract(structured) -> canonical 语料筛查(structured) -> merge_match -> title
    const openai = makeOpenai([
      '```json\n{"要点":"加缓存","要做的事":[]}\n```',
      '```json\n{"candidates":[{"number":57,"kind":"issue","relevance":"direct"}]}\n```',
      'DUPLICATE(#57)',
      'feat: add caching layer'
    ]);
    const ops = makeOps();
    const ctx = {
      historyContext: {
        enrich: jest.fn().mockResolvedValue([{
          number: 57, kind: 'issue', title: '缓存', body: '正文',
          state: 'closed', state_reason: 'completed', closed_at: null,
          comments: [{ author: 'maintainer', body: '方案已定', created_at: '2026-01-01' }],
          timeline: []
        }])
      },
      index: [
        { number: 57, kind: 'issue', title: '缓存', labels: ['canonical'], state: 'closed', state_reason: 'completed', closed_at: null, body: '正文' },
        { number: 58, kind: 'issue', title: '无关', labels: ['canonical'], state: 'closed', state_reason: null, closed_at: null, body: 'b' }
      ]
    };
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: false, enableTwoStage: true }, ops);

    const result = await svc.govern({}, 'o', 'r', pr, null, ctx);

    expect(result).toMatchObject({ decision: GOVERNANCE_DECISIONS.DUPLICATE, canonicalNumber: 57, linked: true });
    // 共享索引：不再走旧 listCanonicalIssues
    expect(ops.listCanonicalIssues).not.toHaveBeenCalled();
    // 阶段一收紧凑 canonical 索引；阶段二语料是深补全的单条候选（含评论历史，R9）
    const screenInput = JSON.parse(openai._create.mock.calls[1][0].messages[1].content);
    expect(screenInput.index.map(i => i.number)).toEqual([57, 58]);
    const matchInput = JSON.parse(openai._create.mock.calls[2][0].messages[1].content);
    expect(matchInput.canonical_index).toHaveLength(1);
    expect(matchInput.canonical_index[0].body).toContain('maintainer: 方案已定');
  });

  test('两段式开启 + ctx + 归并防幻觉：DUPLICATE 引用候选外编号 → 降级 UNCERTAIN 放行', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"加缓存","要做的事":[]}\n```',
      '```json\n{"candidates":[]}\n```',
      'DUPLICATE(#999)'
    ]);
    const ops = makeOps();
    const ctx = {
      historyContext: { enrich: jest.fn() },
      index: [{ number: 57, kind: 'issue', title: '缓存', labels: ['canonical'], state: 'closed', state_reason: null, closed_at: null, body: 'b' }]
    };
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: false, enableTwoStage: true }, ops);

    const result = await svc.govern({}, 'o', 'r', pr, null, ctx);

    expect(result).toMatchObject({ decision: GOVERNANCE_DECISIONS.UNCERTAIN, passed: true });
    expect(ops.updatePullRequest).not.toHaveBeenCalled();
  });

  test('两段式关闭（默认）：行为与原先一致——直接 listCanonicalIssues，无筛查调用', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"加缓存","要做的事":[]}\n```',
      'DUPLICATE(#57)',
      'feat: add caching layer'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '缓存', body: '...' }] });
    const svc = new PrGovernanceService(openai, 'model', config, { dryRun: false }, ops);

    const result = await svc.govern({}, 'o', 'r', pr, null, null);

    expect(result).toMatchObject({ decision: GOVERNANCE_DECISIONS.DUPLICATE, canonicalNumber: 57 });
    expect(ops.listCanonicalIssues).toHaveBeenCalledTimes(1);
    // 3 次调用：extract + merge + title；无筛查插入
    expect(openai._create).toHaveBeenCalledTimes(3);
  });
});
