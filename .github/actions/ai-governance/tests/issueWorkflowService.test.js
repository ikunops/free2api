const baseConfig = require('../config.json');
const { applyLocale } = require('../src/utils/config');
const IssueWorkflowService = require('../src/services/issueWorkflowService');

function buildConfig() {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  return config;
}

// openai stub：按调用顺序返回预设结果
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

// octokit stub：issues 系列 REST 端点
function makeOctokit() {
  return {
    rest: {
      issues: {
        createComment: jest.fn().mockResolvedValue({}),
        update: jest.fn().mockResolvedValue({}),
        lock: jest.fn().mockResolvedValue({}),
        addLabels: jest.fn().mockResolvedValue({})
      },
      repos: {
        getReadme: jest.fn().mockRejectedValue(new Error('no readme'))
      }
    }
  };
}

const issue = {
  number: 22,
  title: '登录失败怎么解决',
  body: '装完之后登录不上去。',
  user: { login: 'someone' }
};

const qualityAnalysis = {
  contentInfo: { userContent: '装完之后登录不上去。', validSections: 0 },
  templateInfo: { hasTemplate: false, templateType: '', confidence: 0 },
  quality: { level: 'low', score: 10 }
};

const labelsList = ['bug', 'enhancement', 'question'];

describe('IssueWorkflowService.classifyAndHandleIssue', () => {
  test('BASIC 问题：评论 issue_basic + 关闭 + 锁定（closeAndLock 真实存在，不再 TypeError）', async () => {
    const config = buildConfig();
    // 依次：分类 -> 内容质量
    const openai = makeOpenai(['BUG', 'BASIC']);
    const octokit = makeOctokit();
    const svc = new IssueWorkflowService(octokit, openai, 'model', config);

    const result = await svc.classifyAndHandleIssue('o', 'r', issue, qualityAnalysis, labelsList);

    expect(result.closed).toBe(true);
    expect(result.classification).toBe('BUG');
    // 评论先于关闭（安全写序）
    expect(octokit.rest.issues.createComment).toHaveBeenCalledTimes(1);
    expect(octokit.rest.issues.createComment.mock.calls[0][0].body).toContain('基础');
    expect(octokit.rest.issues.update).toHaveBeenCalledWith(
      expect.objectContaining({ state: 'closed' })
    );
    expect(octokit.rest.issues.lock).toHaveBeenCalledWith(
      expect.objectContaining({ lock_reason: 'spam' })
    );
  });

  test('UNCLEAR 问题：智能回答/标准提示，不关闭', async () => {
    const config = buildConfig();
    const openai = makeOpenai(['BUG', 'UNCLEAR']);
    const octokit = makeOctokit();
    const svc = new IssueWorkflowService(octokit, openai, 'model', config);

    const result = await svc.classifyAndHandleIssue('o', 'r', issue, qualityAnalysis, labelsList);

    expect(result.needsInfo).toBe(true);
    expect(octokit.rest.issues.update).not.toHaveBeenCalled();
    expect(octokit.rest.issues.createComment).toHaveBeenCalled();
  });

  test('分类抛错：返回 {classification:null, error:true}（FIX-D 将在 handler 消费）', async () => {
    const config = buildConfig();
    const openai = makeOpenai();
    openai._create.mockRejectedValue(new Error('classify boom'));
    const octokit = makeOctokit();
    const svc = new IssueWorkflowService(octokit, openai, 'model', config);

    const result = await svc.classifyAndHandleIssue('o', 'r', issue, qualityAnalysis, labelsList);

    expect(result.error).toBe(true);
    expect(result.classification).toBeNull();
  });
});
