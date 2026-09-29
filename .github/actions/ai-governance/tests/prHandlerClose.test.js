const { handleNewPR } = require('../src/handlers/prHandler');
const { applyLocale } = require('../src/utils/config');
const baseConfig = require('../config.json');

jest.mock('../src/services/prWorkflowService');
jest.mock('../src/services/prGovernanceService');
jest.mock('../src/services/prReviewService');

const PrWorkflowService = require('../src/services/prWorkflowService');

function buildConfig() {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  return config;
}

function makePR() {
  return {
    number: 42,
    title: 'feat: add caching',
    body: 'body',
    user: { login: 'someone' }
  };
}

function makeContext(pr) {
  return { payload: { pull_request: pr, action: 'opened' }, eventName: 'pull_request_target' };
}

function makeOctokit() {
  return {
    rest: {
      repos: { checkCollaborator: jest.fn().mockRejectedValue(new Error('not a collaborator')) },
      pulls: {
        listFiles: jest.fn().mockResolvedValue({ data: [] }),
        update: jest.fn().mockResolvedValue({})
      },
      issues: {
        createComment: jest.fn().mockResolvedValue({}),
        update: jest.fn().mockResolvedValue({}),
        lock: jest.fn().mockResolvedValue({})
      }
    }
  };
}

describe('prHandler 关闭路径的锁定语义（C4）', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    PrWorkflowService.mockImplementation(() => ({
      performLayeredDetection: jest.fn().mockResolvedValue({ decision: 'KEEP' }),
      classifyAndLabelPR: jest.fn().mockResolvedValue('enhancement'),
      config: buildConfig()
    }));
  });

  test('TRIVIAL 关闭：不锁定（pr_trivial 承诺作者可改进后重提，锁定使其不可能）', async () => {
    const config = buildConfig();
    PrWorkflowService.mockImplementation(() => ({
      performLayeredDetection: jest.fn().mockResolvedValue({ decision: 'TRIVIAL' }),
      classifyAndLabelPR: jest.fn().mockResolvedValue('enhancement'),
      config
    }));

    const octokit = makeOctokit();
    await handleNewPR(octokit, {}, makeContext(makePR()), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false, maintainerExempt: false });

    expect(octokit.rest.pulls.update).toHaveBeenCalledWith(expect.objectContaining({ state: 'closed' }));
    expect(octokit.rest.issues.createComment).toHaveBeenCalled();
    // TRIVIAL：不锁
    expect(octokit.rest.issues.lock).not.toHaveBeenCalled();
  });

  test('MALICIOUS 关闭：仍锁定（防御性，文档化决策 R4）', async () => {
    const config = buildConfig();
    PrWorkflowService.mockImplementation(() => ({
      performLayeredDetection: jest.fn().mockResolvedValue({ decision: 'MALICIOUS' }),
      classifyAndLabelPR: jest.fn().mockResolvedValue('enhancement'),
      config
    }));

    const octokit = makeOctokit();
    await handleNewPR(octokit, {}, makeContext(makePR()), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false, maintainerExempt: false });

    expect(octokit.rest.pulls.update).toHaveBeenCalledWith(expect.objectContaining({ state: 'closed' }));
    expect(octokit.rest.issues.lock).toHaveBeenCalled();
  });

  test('内容过滤关闭：不锁定（与 issue 路径对齐 —— 回应承诺可编辑后重新提交）', async () => {
    const config = buildConfig();
    // prGovernance 抛内容过滤错误 → handler 走 pr_content_filtered 关闭
    const PrGovernanceService = require('../src/services/prGovernanceService');
    PrGovernanceService.mockImplementation(() => ({
      govern: jest.fn().mockImplementation(() => {
        const e = new Error('Provider failed content_filter');
        e.status = 400;
        e.error = { reason: 'content_filter' };
        throw e;
      })
    }));

    const octokit = makeOctokit();
    await handleNewPR(octokit, {}, makeContext(makePR()), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false, maintainerExempt: false });

    expect(octokit.rest.pulls.update).toHaveBeenCalledWith(expect.objectContaining({ state: 'closed' }));
    expect(octokit.rest.issues.createComment).toHaveBeenCalled();
    // 内容过滤（AI 提供商误伤）：不锁，保留「编辑后重提」的出路
    expect(octokit.rest.issues.lock).not.toHaveBeenCalled();
  });

  test('SPAM 关闭：仍锁定', async () => {
    const config = buildConfig();
    PrWorkflowService.mockImplementation(() => ({
      performLayeredDetection: jest.fn().mockResolvedValue({ decision: 'SPAM' }),
      classifyAndLabelPR: jest.fn().mockResolvedValue('enhancement'),
      config
    }));

    const octokit = makeOctokit();
    await handleNewPR(octokit, {}, makeContext(makePR()), 'o', 'r', 'model', config, ['enhancement'], [], { dryRun: false, maintainerExempt: false });

    expect(octokit.rest.issues.lock).toHaveBeenCalled();
  });
});
