const baseConfig = require('../config.json');
const { applyLocale } = require('../src/utils/config');
const IssueGovernanceService = require('../src/services/issueGovernanceService');
const { GOVERNANCE_DECISIONS } = require('../src/utils/constants');

function buildConfig() {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  return config;
}

// 构造一个 openai stub：chat.completions.create 按 purpose 序列返回预设结果
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
    createIssue: jest.fn().mockResolvedValue({ data: { number: 99, html_url: 'https://github.com/o/r/issues/99' } }),
    addComment: jest.fn().mockResolvedValue({}),
    addLabels: jest.fn().mockResolvedValue({}),
    updateIssueState: jest.fn().mockResolvedValue({})
  };
}

const issue = {
  number: 22,
  title: '请求支持某种能力',
  body: '希望网关支持 xxx，目前做不到。',
  user: { login: 'someone' }
};

describe('IssueGovernanceService', () => {
  test('DUPLICATE 归并：打 duplicate 标签、评论、关闭原 issue，并在 canonical 追加记录', async () => {
    const config = buildConfig();
    // 依次: extract(structured) -> well_formed -> merge_match
    const openai = makeOpenai([
      '```json\n{"要点":"支持 xxx","要做的事":["实现 xxx"]}\n```',
      'WELL_FORMED',
      'DUPLICATE(#57)'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: '已有 xxx 能力', body: '...' }] });
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false }, ops);
    const octokit = {}; // ops 是 mock，octokit 不会被真正调用

    const result = await gov.govern(octokit, 'o', 'r', issue, 'enhancement');

    expect(result).toMatchObject({
      decision: GOVERNANCE_DECISIONS.DUPLICATE,
      canonicalNumber: 57,
      closed: true
    });
    expect(ops.addLabels).toHaveBeenCalledWith(octokit, 'o', 'r', 22, ['duplicate'], expect.any(String));
    expect(ops.updateIssueState).toHaveBeenCalledWith(octokit, 'o', 'r', 22, 'closed', 'not_planned');
    // canonical 上追加归并记录：来源 issue 编号、作者、要点
    const recordCall = ops.addComment.mock.calls.find(c => c[3] === 57);
    expect(recordCall).toBeTruthy();
    expect(recordCall[4]).toContain('#22');
    expect(recordCall[4]).toContain('@someone');
    // 原 issue 评论指向 canonical
    const mergeCall = ops.addComment.mock.calls.find(c => c[3] === 22);
    expect(mergeCall[4]).toContain('#57');
  });

  test('NEW_TOPIC 且非规范：创建 canonical，关闭原 issue', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"新需求 yyy","要做的事":["做 yyy"]}\n```',
      'NEEDS_NORMALIZE',
      'NEW_TOPIC',
      '[Feature] 新需求 yyy\n\n## 概述\n支持 yyy。\n\n## 背景与要点\n- 做 yyy'
    ]);
    const ops = makeOps();
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false }, ops);
    const octokit = {};

    const result = await gov.govern(octokit, 'o', 'r', issue, 'enhancement');

    expect(result.decision).toBe(GOVERNANCE_DECISIONS.NEW_TOPIC);
    expect(ops.createIssue).toHaveBeenCalled();
    const createArgs = ops.createIssue.mock.calls[0];
    expect(createArgs[3]).toMatch(/^\[Feature\]/); // 标题带前缀
    // 创建 canonical 成功，才允许关闭原 issue
    expect(ops.updateIssueState).toHaveBeenCalledWith(octokit, 'o', 'r', 22, 'closed', 'not_planned');
  });

  test('创建 canonical 失败时绝不关闭原 issue（先建后关的顺序保证）', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"新需求","要做的事":[]}\n```',
      'NEEDS_NORMALIZE',
      'NEW_TOPIC',
      '[Feature] x\n\n## 概述\nx'
    ]);
    const ops = makeOps();
    ops.createIssue.mockRejectedValueOnce(new Error('boom'));
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false }, ops);

    await expect(gov.govern({}, 'o', 'r', issue, null)).rejects.toThrow('boom');
    expect(ops.updateIssueState).not.toHaveBeenCalled();
  });

  test('UNCERTAIN 时谨慎放行：只评论，不关闭', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"模糊","要做的事":[]}\n```',
      'NEEDS_NORMALIZE',
      'UNCERTAIN'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 1, title: 'x', body: 'x' }] });
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false }, ops);
    const octokit = {};

    const result = await gov.govern(octokit, 'o', 'r', issue, null);

    expect(result.decision).toBe(GOVERNANCE_DECISIONS.UNCERTAIN);
    expect(result.passed).toBe(true);
    expect(ops.updateIssueState).not.toHaveBeenCalled();
    expect(ops.addComment).toHaveBeenCalled();
  });

  test('dry-run 模式：不关闭、不创建、不打标签，仍执行分析与评论', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"重复需求","要做的事":[]}\n```',
      'NEEDS_NORMALIZE',
      'DUPLICATE(#57)'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: 'x', body: 'x' }] });
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: true }, ops);
    const octokit = {};

    const result = await gov.govern(octokit, 'o', 'r', issue, null);

    expect(result.dryRun).toBe(true);
    expect(ops.createIssue).not.toHaveBeenCalled();
    expect(ops.updateIssueState).not.toHaveBeenCalled();
    expect(ops.addLabels).not.toHaveBeenCalled();
    // dry-run 仍会发一条说明评论
    expect(ops.addComment).toHaveBeenCalled();
  });

  test('canonical 索引为空时直接视为新主题，不发起归并匹配调用', async () => {
    const config = buildConfig();
    // 若跳过匹配，第三段 NEW_TOPIC 不应被消费；这里只准备 extract + well_formed 两段
    const openai = makeOpenai([
      '```json\n{"要点":"a","要做的事":[]}\n```',
      'NEEDS_NORMALIZE'
    ]);
    const ops = makeOps({ canonicalItems: [] });
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: true }, ops);
    const octokit = {};

    await gov.govern(octokit, 'o', 'r', issue, null);

    expect(openai._create).toHaveBeenCalledTimes(2); // extract + well_formed
  });

  test('WELL_FORMED：发 AI 生成的实质性评审评论（认可/方案/后续），不打架固定模板', async () => {
    const config = buildConfig();
    const review = [
      '感谢提交这份高质量的 issue @someone 👍',
      '',
      '## 分析认可',
      '- 明确指出了 xxx 场景下的缺口',
      '',
      '## 实现方案',
      '方案可落地，关键点：',
      '1. 新增配置项',
      '',
      '## 后续',
      '欢迎直接提 PR，基准分支 `main`，建议：',
      '- PR 描述中关联本 issue（`Closes #22` 或 `Refs #22`）；'
    ].join('\n');
    // 依次: extract(structured) -> well_formed 判定 -> 评审评论生成
    const openai = makeOpenai([
      '```json\n{"要点":"a","要做的事":[]}\n```',
      'WELL_FORMED',
      review
    ]);
    const ops = makeOps({ canonicalItems: [] });
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false }, ops);
    const octokit = {};

    const result = await gov.govern(octokit, 'o', 'r', issue, 'enhancement');

    expect(result).toMatchObject({ decision: 'WELL_FORMED', promoted: true });
    // 原地打 canonical + 分类标签，不关闭
    expect(ops.addLabels).toHaveBeenCalledWith(octokit, 'o', 'r', 22, ['canonical', 'enhancement'], expect.any(String));
    expect(ops.updateIssueState).not.toHaveBeenCalled();
    // 评论是 AI 生成的实质内容，且保留机器人操作日志行
    const commentCall = ops.addComment.mock.calls.find(c => c[3] === 22);
    expect(commentCall).toBeTruthy();
    expect(commentCall[4]).toContain('## 分析认可');
    expect(commentCall[4]).toContain('## 实现方案');
    expect(commentCall[4]).toContain('## 后续');
    expect(commentCall[4]).toContain('@someone');
    // 机器人身份标记由服务端确定性补上（开头 🤖 + 结尾操作日志行）
    expect(commentCall[4].startsWith('🤖')).toBe(true);
    expect(commentCall[4]).toContain('✅ Claude Code 操作日志：');
  });

  test('WELL_FORMED 但 AI 评审生成失败：回落固定模板评论，流程不中断', async () => {
    const config = buildConfig();
    // 依次: extract -> well_formed 判定 -> 评审生成抛错
    const openai = makeOpenai([
      '```json\n{"要点":"a","要做的事":[]}\n```',
      'WELL_FORMED'
    ]);
    openai._create.mockRejectedValueOnce(new Error('review boom'));
    const ops = makeOps({ canonicalItems: [] });
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false }, ops);
    const octokit = {};

    const result = await gov.govern(octokit, 'o', 'r', issue, null);

    expect(result).toMatchObject({ decision: 'WELL_FORMED', promoted: true });
    expect(ops.addLabels).toHaveBeenCalledWith(octokit, 'o', 'r', 22, ['canonical'], expect.any(String));
    // 回落到固定模板 + 日志行
    const commentCall = ops.addComment.mock.calls.find(c => c[3] === 22);
    expect(commentCall).toBeTruthy();
    expect(commentCall[4]).toContain('已按模板规范填写');
    expect(commentCall[4]).toContain('✅ Claude Code 操作日志：');
  });

  test('WELL_FORMED + dry-run：仍生成 AI 评审评论并演练发布，不做任何写操作', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"a","要做的事":[]}\n```',
      'WELL_FORMED',
      '感谢提交 @someone\n\n## 分析认可\n- 好\n\n## 实现方案\n可行\n\n## 后续\n欢迎提 PR'
    ]);
    const ops = makeOps({ canonicalItems: [] });
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: true }, ops);
    const octokit = {};

    const result = await gov.govern(octokit, 'o', 'r', issue, null);

    expect(result).toMatchObject({ decision: 'WELL_FORMED', dryRun: true });
    expect(ops.addLabels).not.toHaveBeenCalled();
    expect(ops.updateIssueState).not.toHaveBeenCalled();
    // dry-run 也走 AI 评审评论路径
    const commentCall = ops.addComment.mock.calls.find(c => c[3] === 22);
    expect(commentCall[4]).toContain('## 分析认可');
    expect(commentCall[4].startsWith('🤖')).toBe(true);
  });

  // ---- F2/F3：两段式归并匹配 + 规范评审评论的历史引用 ----

  test('两段式开启：归并匹配经筛选 AI 选候选，阶段二只看深补全的候选（R9：含评论历史）', async () => {
    const config = buildConfig();
    // 调用顺序：extract(structured) -> well_formed -> screening(structured) -> merge_match
    const openai = makeOpenai([
      '```json\n{"要点":"支持 xxx","要做的事":[]}\n```',
      'NEEDS_NORMALIZE',
      '```json\n{"candidates":[{"number":57,"kind":"issue","relevance":"direct"}]}\n```',
      'DUPLICATE(#57)'
    ]);
    const ops = makeOps();
    const ctx = {
      historyContext: {
        enrich: jest.fn().mockResolvedValue([{
          number: 57, kind: 'issue', title: '已有 xxx', body: '正文',
          state: 'closed', state_reason: 'completed', closed_at: null,
          comments: [{ author: 'maintainer', body: '已经支持了', created_at: '2026-01-01' }],
          timeline: []
        }])
      },
      index: [
        { number: 57, kind: 'issue', title: '已有 xxx', labels: ['canonical'], state: 'closed', state_reason: 'completed', closed_at: null, body: '正文' },
        { number: 58, kind: 'issue', title: '无关主题', labels: ['canonical'], state: 'closed', state_reason: null, closed_at: null, body: 'b' }
      ]
    };
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false, enableTwoStage: true }, ops);

    const result = await gov.govern({}, 'o', 'r', issue, 'enhancement', ctx);

    expect(result).toMatchObject({ decision: GOVERNANCE_DECISIONS.DUPLICATE, canonicalNumber: 57, closed: true });
    // 旧通道不再拉取（共享索引）
    expect(ops.listCanonicalIssues).not.toHaveBeenCalled();
    // 阶段一只收紧凑 canonical 索引
    const screenInput = JSON.parse(openai._create.mock.calls[2][0].messages[1].content);
    expect(screenInput.index.map(i => i.number)).toEqual([57, 58]);
    expect('body' in screenInput.index[0]).toBe(false);
    // 阶段二 merge_match 语料含候选的评论历史（R9）
    const matchInput = JSON.parse(openai._create.mock.calls[3][0].messages[1].content);
    expect(matchInput.canonical_index).toHaveLength(1);
    expect(matchInput.canonical_index[0].body).toContain('maintainer: 已经支持了');
    expect(ctx.historyContext.enrich).toHaveBeenCalledWith('o', 'r', [{ number: 57, kind: 'issue' }]);
  });

  test('两段式 + 筛选为空：回落全量 canonical 索引（截断口径同旧行为），不深补全', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"支持 xxx","要做的事":[]}\n```',
      'NEEDS_NORMALIZE',
      '```json\n{"candidates":[]}\n```',
      'NEW_TOPIC',
      '[Feature] 新需求\n\n## 概述\nx'
    ]);
    const ops = makeOps();
    const ctx = {
      historyContext: { enrich: jest.fn() },
      index: [{ number: 57, kind: 'issue', title: '已有 xxx', labels: ['canonical'], state: 'closed', state_reason: null, closed_at: null, body: '正文' }]
    };
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false, enableTwoStage: true }, ops);

    const result = await gov.govern({}, 'o', 'r', issue, null, ctx);

    expect(result.decision).toBe(GOVERNANCE_DECISIONS.NEW_TOPIC);
    expect(ctx.historyContext.enrich).not.toHaveBeenCalled();
    const matchInput = JSON.parse(openai._create.mock.calls[3][0].messages[1].content);
    expect(matchInput.canonical_index).toEqual([
      { number: 57, title: '已有 xxx', body: '正文', state: 'closed', state_reason: null, closed_at: null }
    ]);
  });

  test('两段式 + 无 ctx：回落旧 listCanonicalIssues 通道（向后兼容）', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"支持 xxx","要做的事":[]}\n```',
      'NEEDS_NORMALIZE',
      'DUPLICATE(#57)'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: 'x', body: 'x' }] });
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false, enableTwoStage: true }, ops);

    const result = await gov.govern({}, 'o', 'r', issue, null, null);

    expect(result).toMatchObject({ decision: GOVERNANCE_DECISIONS.DUPLICATE, canonicalNumber: 57 });
    expect(ops.listCanonicalIssues).toHaveBeenCalled();
  });

  test('归并防幻觉闸门：DUPLICATE(#N) 引用语料外编号 → 降级 UNCERTAIN 放行，不关闭', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"要点":"支持 xxx","要做的事":[]}\n```',
      'NEEDS_NORMALIZE',
      'DUPLICATE(#999)'
    ]);
    const ops = makeOps({ canonicalItems: [{ number: 57, title: 'x', body: 'x' }] });
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false }, ops);

    const result = await gov.govern({}, 'o', 'r', issue, null);

    expect(result).toMatchObject({ decision: GOVERNANCE_DECISIONS.UNCERTAIN, passed: true });
    expect(ops.updateIssueState).not.toHaveBeenCalled();
    expect(ops.addLabels).not.toHaveBeenCalled();
  });

  test('WELL_FORMED + 两段式：评审评论输入携带 related_history（筛查命中的历史 issue+PR，C10）', async () => {
    const config = buildConfig();
    const review = '感谢提交 @someone\n\n## 分析认可\n- 好\n\n## 实现方案\n参考 #57 的结论\n\n## 后续\n欢迎提 PR';
    // 调用顺序：extract -> well_formed -> well_formed 评审的历史筛查(structured) -> 评审评论
    const openai = makeOpenai([
      '```json\n{"要点":"a","要做的事":[]}\n```',
      'WELL_FORMED',
      '```json\n{"candidates":[{"number":57,"kind":"issue","relevance":"direct"},{"number":30,"kind":"pr","relevance":"context"}]}\n```',
      review
    ]);
    const ops = makeOps();
    const ctx = {
      historyContext: { enrich: jest.fn() },
      // #57 不带 canonical 标签 → canonical 语料为空 → 跳过归并匹配，直接进规范评审路径
      index: [
        { number: 57, kind: 'issue', title: '缓存', labels: [], state: 'closed', state_reason: 'not_planned', closed_at: null, body: 'b' },
        { number: 30, kind: 'pr', title: 'feat: 缓存', labels: [], state: 'merged', state_reason: null, closed_at: null, body: 'b' }
      ]
    };
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false, enableTwoStage: true }, ops);

    const result = await gov.govern({}, 'o', 'r', issue, 'enhancement', ctx);

    expect(result).toMatchObject({ decision: 'WELL_FORMED', promoted: true });
    // 评审评论提示词输入含 related_history（issue + PR 都在，C10）
    const reviewInput = JSON.parse(openai._create.mock.calls[3][0].messages[1].content);
    expect(reviewInput.related_history).toHaveLength(2);
    expect(reviewInput.related_history.map(h => h.number)).toEqual([57, 30]);
    expect(reviewInput.related_history.find(h => h.number === 57)).toMatchObject({ kind: 'issue', state_reason: 'not_planned' });
    // 历史筛查在全量索引上进行（不过滤 canonical）
    const screenInput = JSON.parse(openai._create.mock.calls[2][0].messages[1].content);
    expect(screenInput.index).toHaveLength(2);
    // 评论携带历史指引行（引用了真实历史）
    const commentCall = ops.addComment.mock.calls.find(c => c[3] === 22);
    expect(commentCall[4]).toContain(config.responses.governance_history_reference_note);
  });

  test('WELL_FORMED 评审引用闸门：草稿引用历史集合外的编号 → 回落固定模板评论', async () => {
    const config = buildConfig();
    const hallucinated = '感谢提交 @someone\n\n## 分析认可\n- 好\n\n## 实现方案\n参考 #999 的结论\n\n## 后续\n欢迎提 PR';
    const openai = makeOpenai([
      '```json\n{"要点":"a","要做的事":[]}\n```',
      'WELL_FORMED',
      '```json\n{"candidates":[{"number":57,"kind":"issue","relevance":"direct"}]}\n```',
      hallucinated
    ]);
    const ops = makeOps();
    const ctx = {
      historyContext: { enrich: jest.fn() },
      index: [{ number: 57, kind: 'issue', title: '缓存', labels: [], state: 'closed', state_reason: 'not_planned', closed_at: null, body: 'b' }]
    };
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false, enableTwoStage: true }, ops);

    const result = await gov.govern({}, 'o', 'r', issue, null, ctx);

    expect(result).toMatchObject({ decision: 'WELL_FORMED', promoted: true });
    const commentCall = ops.addComment.mock.calls.find(c => c[3] === 22);
    expect(commentCall[4]).toContain('已按模板规范填写'); // 固定模板回落
    expect(commentCall[4]).not.toContain('#999');
  });

  test('WELL_FORMED + 两段式 + 筛查为空：评审评论不带历史引用，与原行为一致', async () => {
    const config = buildConfig();
    const review = '感谢提交 @someone\n\n## 分析认可\n- 好\n\n## 实现方案\n可行\n\n## 后续\n欢迎提 PR';
    const openai = makeOpenai([
      '```json\n{"要点":"a","要做的事":[]}\n```',
      'WELL_FORMED',
      '```json\n{"candidates":[]}\n```',
      review
    ]);
    const ops = makeOps();
    const ctx = {
      historyContext: { enrich: jest.fn() },
      index: [{ number: 57, kind: 'issue', title: 'x', labels: [], state: 'closed', state_reason: null, closed_at: null, body: 'b' }]
    };
    const gov = new IssueGovernanceService(openai, 'model', config, { dryRun: false, enableTwoStage: true }, ops);

    await gov.govern({}, 'o', 'r', issue, null, ctx);

    const reviewInput = JSON.parse(openai._create.mock.calls[3][0].messages[1].content);
    expect(reviewInput.related_history).toEqual([]);
    const commentCall = ops.addComment.mock.calls.find(c => c[3] === 22);
    expect(commentCall[4]).not.toContain(config.responses.governance_history_reference_note);
  });
});
