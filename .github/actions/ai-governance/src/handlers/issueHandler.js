const core = require('@actions/core');
const { logMessage } = require('../utils/helpers');
const { getReadmeContent, getPinnedIssuesContent, addComment } = require('../services/github');
const { analyzeIssueQuality, generateAnalysisReport } = require('../services/templateDetector');
const IssueWorkflowService = require('../services/issueWorkflowService');
const IssueGovernanceService = require('../services/issueGovernanceService');
const { isContentFilterError } = require('../services/ai');
const { GOVERNANCE_DEFAULTS } = require('../utils/constants');
const {
  handleSpamIssue,
  handleContentFilteredIssue,
  handleBlacklistedUser
} = require('./issueProcessor');

/**
 * 处理新创建的 Issue。
 *
 * 链路（详见 DESIGN.md）：
 *   黑名单 → bot 自环豁免 → 垃圾检测 → README/置顶覆盖 → 内容分级(UNCLEAR/BASIC)
 *   → 分类打标 → 维护者豁免 → 治理（要点提炼 / 规范判定 / canonical 归并 / 规范化重开）。
 *
 * @param {Object} octokit GitHub API 客户端
 * @param {Object} openai OpenAI 客户端
 * @param {Object} context GitHub 上下文
 * @param {string} owner 仓库所有者
 * @param {string} repo 仓库名
 * @param {string} aiModel AI 模型名
 * @param {Object} config 合并后的配置
 * @param {Array} labelsList 分类标签列表
 * @param {Array} blacklistUsers 黑名单用户列表
 * @param {Object} gov 治理参数（与 GOVERNANCE_DEFAULTS 合并）
 */
async function handleNewIssue(octokit, openai, context, owner, repo, aiModel, config, labelsList, blacklistUsers, gov = {}) {
  gov = { ...GOVERNANCE_DEFAULTS, ...gov };
  // 跳过名单合并：默认永远豁免 bot 自身（避免治理自身创建的 canonical 造成无限循环）
  gov.skipUsers = [...new Set([...(GOVERNANCE_DEFAULTS.SKIP_USERS || []), ...(gov.skipUsers || [])])];

  try {
    const issue = context.payload.issue;
    const issueTitle = issue.title;
    const issueBody = issue.body || '';
    const issueAuthor = (issue.user && issue.user.login || '').toLowerCase();

    core.info(logMessage(config.logging.issue_check_start, { title: issueTitle }));
    core.info(logMessage(config.logging.target_repo, { owner, repo }));

    // bot 自环豁免
    if (gov.skipUsers.includes(issueAuthor)) {
      core.warning(logMessage(config.logging.governance_skip_user, { number: issue.number }));
      return;
    }

    // 检查用户是否在黑名单中
    if (blacklistUsers.includes(issueAuthor)) {
      await handleBlacklistedUser(octokit, owner, repo, issue, config);
      return;
    }

    // 安全阀：维护者/协作者提交的 issue 跳过治理（保留垃圾检测 + 分类，但不关闭、不重开、不归并）
    let skipGovernance = false;
    if (gov.maintainerExempt) {
      const isMaintainer = await isCollaborator(octokit, owner, repo, issueAuthor);
      if (isMaintainer) {
        core.info(logMessage(config.logging.governance_skip_maintainer, { number: issue.number }));
        skipGovernance = true;
      }
    }

    // 创建工作流服务实例（保留上游垃圾检测 / README 覆盖 / 内容分级能力）
    const workflowService = new IssueWorkflowService(octokit, openai, aiModel, config);

    // 获取README.md内容
    const readmeContent = await getReadmeContent(octokit, owner, repo, config);
    core.info(readmeContent ? config.logging.readme_found : config.logging.readme_not_found);

    // 获取置顶Issues内容
    const pinnedIssuesContent = await getPinnedIssuesContent(octokit, owner, repo, config);
    core.info(pinnedIssuesContent ? config.logging.pinned_issues_found : config.logging.pinned_issues_not_found);

    // 智能分析Issue内容质量和模板使用情况
    const qualityAnalysis = analyzeIssueQuality(issueTitle, issueBody);
    const templateAnalysisReport = generateAnalysisReport(
      qualityAnalysis.templateInfo,
      qualityAnalysis.contentInfo
    );
    logTemplateDetectionInfo(qualityAnalysis, config);

    // 调用AI进行分层检测（垃圾 → README/置顶覆盖 → 通过）
    const analysisResult = await workflowService.performLayeredDetection(
      issue,
      readmeContent,
      pinnedIssuesContent,
      templateAnalysisReport
    );
    const decision = analysisResult.decision;

    if (decision === 'SPAM') {
      await handleSpamIssue(octokit, owner, repo, issue, config);
      return;
    }

    if (decision === 'README_COVERED') {
      // README相关的Issue：先回答，再关闭但不锁定（保留上游能力）
      await workflowService.handleReadmeRelatedIssue(owner, repo, issue, readmeContent);
      return;
    }

    // 内容分级 + 分类打标（保留上游 classification / UNCLEAR / BASIC 能力）
    const triage = await workflowService.classifyAndHandleIssue(owner, repo, issue, qualityAnalysis, labelsList);
    if (triage.closed) {
      // BASIC 等需要关闭的情形已处理
      return;
    }
    if (triage.needsInfo) {
      // UNCLEAR：已要求补充信息，此处停留，不进入治理
      return;
    }
    if (triage.error) {
      // 分诊失败（FIX-D/C8）：绝不能带着 classification=null 流入治理 ——
      // splitCanonical 会把崩溃的 bug 报告误前缀成 [Feature]，污染 canonical 索引。
      // 宁可漏判：fail-open 评论后放行，维护者可重跑。
      core.warning(logMessage(config.logging.classification_failed, { error: 'triage error, skip governance' }));
      await failOpenComment(octokit, owner, repo, issue, config);
      return;
    }
    const classification = triage.classification;

    if (skipGovernance) {
      // 维护者豁免：不再做归并/重开
      core.info(logMessage(config.logging.issue_passed_log, { number: issue.number }));
      return;
    }

    // 治理层：要点提炼 + 归并匹配 + 规范化重开。
    // F3：两段式开启时构建共享历史索引单次拉取（issue+PR 语料，C5/R5），传入治理与评审；
    //      关闭时 ctx=null，治理服务自取 canonical 索引，行为与原先完全一致。
    let sharedCtx = null;
    if (gov.enableTwoStage) {
      const HistoryContextService = require('../services/historyContextService');
      const historyContext = new HistoryContextService(octokit, config, gov);
      const index = await historyContext.buildIndex(owner, repo);
      sharedCtx = { historyContext, index };
    }

    // 注：classification 来自 AI 归一化（大写），这里回落到仓库 label 的真实大小写
    const governanceService = new IssueGovernanceService(openai, aiModel, config, gov);
    await governanceService.govern(octokit, owner, repo, issue, resolveLabel(classification, labelsList), sharedCtx);

  } catch (error) {
    core.error(logMessage(config.logging.issue_process_error, { error: error.message }));

    if (isContentFilterError(error)) {
      const payloadIssue = context.payload.issue;
      core.warning(logMessage(config.logging.ai_content_filtered, { number: payloadIssue.number }));
      await handleContentFilteredIssue(octokit, owner, repo, payloadIssue, config);
      return;
    }

    // 安全阀：任何治理失败都不误关 issue —— 仅评论说明并放行
    await failOpenComment(octokit, owner, repo, context.payload.issue, config);
    core.error(config.logging.governance_ai_fallback_log);
  }
}

/**
 * 把 AI 返回的（可能大写的）分类结果回落到仓库标签的真实大小写，避免打出不存在的大写标签。
 */
function resolveLabel(classification, labelsList) {
  if (!classification) {
    return null;
  }
  const hit = labelsList.find(l => l.toLowerCase() === classification.toLowerCase());
  return hit || classification;
}

/**
 * 判断作者是否为仓库协作者（维护者豁免用）。
 * 查不到 / 非协作者都返回 false（宁可多治理，也不因权限查询失败而跳过真正需要治理的 issue）。
 */
async function isCollaborator(octokit, owner, repo, author) {
  if (!author) {
    return false;
  }
  try {
    await octokit.rest.repos.checkCollaborator({
      owner,
      repo,
      username: author
    });
    return true;
  } catch (_error) {
    return false;
  }
}

/**
 * 治理失败兜底：只评论不关闭，明确告知「放过」。
 */
async function failOpenComment(octokit, owner, repo, issue, config) {
  const body = config.responses.governance_ai_fallback_comment;
  try {
    await addComment(
      octokit,
      owner,
      repo,
      issue.number,
      body,
      config.logging.governance_comment_failed
    );
  } catch (commentError) {
    core.warning(logMessage(config.logging.governance_comment_failed, { error: commentError.message }));
  }
}

/**
 * 记录模板检测信息
 */
function logTemplateDetectionInfo(qualityAnalysis, config) {
  if (qualityAnalysis.templateInfo.hasTemplate) {
    core.info(logMessage(config.logging.template_detected, {
      type: qualityAnalysis.templateInfo.templateType,
      confidence: qualityAnalysis.templateInfo.confidence.toFixed(1)
    }));

    core.info(logMessage(config.logging.template_analysis, {
      sections: qualityAnalysis.contentInfo.validSections
    }));

    if (qualityAnalysis.contentInfo.userContent) {
      core.info(logMessage(config.logging.user_content_extracted, {
        length: qualityAnalysis.contentInfo.userContent.length
      }));
    }
  }

  core.info(logMessage(config.logging.quality_analysis, {
    level: qualityAnalysis.quality.level,
    score: qualityAnalysis.quality.score
  }));
}

module.exports = {
  handleNewIssue,
  isCollaborator,
  resolveLabel
};