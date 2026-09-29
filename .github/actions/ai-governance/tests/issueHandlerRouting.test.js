const { handleNewIssue } = require('../src/handlers/issueHandler');
const { applyLocale } = require('../src/utils/config');
const baseConfig = require('../config.json');

jest.mock('../src/services/issueWorkflowService');
jest.mock('../src/services/issueGovernanceService');

const IssueWorkflowService = require('../src/services/issueWorkflowService');
const IssueGovernanceService = require('../src/services/issueGovernanceService');

function buildConfig() {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  return config;
}

function makeIssue(overrides = {}) {
  return {
    number: 22,
    title: '请求支持某种能力',
    body: '希望网关支持 xxx，目前做不到。',
    user: { login: 'someone' },
    ...overrides
  };
}

function makeContext(issue) {
  return { payload: { issue, action: 'opened' }, eventName: 'issues' };
}

function makeOctokit() {
  return {
    rest: {
      repos: {
        checkCollaborator: jest.fn().mockRejectedValue(new Error('not a collaborator')),
        getContent: jest.fn().mockRejectedValue(new Error('no readme'))
      },
      graphql: jest.fn().mockResolvedValue({ repository: { pinnedIssues: { nodes: [] } } }),
      issues: {
        createComment: jest.fn().mockResolvedValue({}),
        update: jest.fn().mockResolvedValue({}),
        lock: jest.fn().mockResolvedValue({}),
        addLabels: jest.fn().mockResolvedValue({})
      }
    }
  };
}

function mockWorkflow({ decision = 'KEEP', triage = { classification: 'enhancement', needsInfo: false, closed: false } } = {}) {
  IssueWorkflowService.mockImplementation(() => ({
    performLayeredDetection: jest.fn().mockResolvedValue({ decision }),
    classifyAndHandleIssue: jest.fn().mockResolvedValue(triage),
    config: buildConfig()
  }));
}

describe('issueHandler 路由（C17 缺口 + FIX-D）', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    mockWorkflow();
    IssueGovernanceService.mockImplementation(() => ({
      govern: jest.fn().mockResolvedValue({})
    }));
  });

  test('triage.error：跳过治理，发 fail-open 评论，不关闭 issue（FIX-D/C8/R8）', async () => {
    const config = buildConfig();
    // 分类 AI 失败 → {classification:null, error:true}
    mockWorkflow({ triage: { classification: null, error: true, needsInfo: false, closed: false } });

    const octokit = makeOctokit();
    await handleNewIssue(octokit, {}, makeContext(makeIssue()), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false });

    // 不进治理层（错误分诊绝不能流入 canonical 归并 —— 会把崩溃的 bug 误前缀成 [Feature]）
    expect(IssueGovernanceService).not.toHaveBeenCalled();
    // fail-open 评论
    expect(octokit.rest.issues.createComment).toHaveBeenCalled();
    // 永不关闭
    expect(octokit.rest.issues.update).not.toHaveBeenCalledWith(expect.objectContaining({ state: 'closed' }));
  });

  test('正常分诊：治理层被调用', async () => {
    const config = buildConfig();
    const octokit = makeOctokit();
    await handleNewIssue(octokit, {}, makeContext(makeIssue()), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false });

    expect(IssueGovernanceService).toHaveBeenCalled();
  });

  test('维护者豁免：检测/分诊保留，治理跳过', async () => {
    const config = buildConfig();
    const octokit = makeOctokit();
    octokit.rest.repos.checkCollaborator.mockResolvedValue({ status: 204 });

    await handleNewIssue(octokit, {}, makeContext(makeIssue()), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false, maintainerExempt: true });

    expect(IssueWorkflowService).toHaveBeenCalled();
    expect(IssueGovernanceService).not.toHaveBeenCalled();
  });

  test('skipUsers（bot 自环）：直接返回，不做任何处理', async () => {
    const config = buildConfig();
    const octokit = makeOctokit();
    const issue = makeIssue({ user: { login: 'github-actions[bot]' } });

    await handleNewIssue(octokit, {}, makeContext(issue), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false });

    expect(IssueWorkflowService).not.toHaveBeenCalled();
    expect(IssueGovernanceService).not.toHaveBeenCalled();
  });

  test('triage.closed（BASIC 已处理）：不进治理（FIX-A 修复后路径可达）', async () => {
    const config = buildConfig();
    mockWorkflow({ triage: { classification: 'bug', closed: true } });

    const octokit = makeOctokit();
    await handleNewIssue(octokit, {}, makeContext(makeIssue()), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false });

    expect(IssueGovernanceService).not.toHaveBeenCalled();
  });

  // ---- F3：两段式共享历史语境接线（issue 路径）----

  test('两段式开启：构建共享索引一次并传给治理层；两段式关闭时传 null', async () => {
    const config = buildConfig();
    const governCalls = [];

    IssueGovernanceService.mockImplementation(() => ({
      govern: jest.fn(async (...args) => {
        governCalls.push(args);
        return {};
      })
    }));

    // 开启
    const octokit = makeOctokit();
    await handleNewIssue(octokit, {}, makeContext(makeIssue()), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false, enableTwoStage: true });
    expect(governCalls[0][5]).toBeTruthy();
    expect(Array.isArray(governCalls[0][5].index)).toBe(true);
    expect(governCalls[0][5].historyContext).toBeTruthy();

    // 关闭（默认）
    const octokit2 = makeOctokit();
    await handleNewIssue(octokit2, {}, makeContext(makeIssue()), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false });
    expect(governCalls[1][5]).toBeNull();
  });
});
