const core = require('@actions/core');
const { logMessage, handleApiCall } = require('../utils/helpers');
const PrWorkflowService = require('../services/prWorkflowService');
const PrGovernanceService = require('../services/prGovernanceService');
const PrReviewService = require('../services/prReviewService');
const { isContentFilterError } = require('../services/ai');
const { closePR, addComment } = require('../services/github');
const { GOVERNANCE_DEFAULTS } = require('../utils/constants');

/**
 * 处理新创建的PR。
 *
 * 链路（详见 DESIGN.md「§PR 治理」）：
 *   黑名单 → 垃圾检测（SPAM/恶意/trivial → 关闭）
 *   → 有效 PR 分类打标 → 维护者/跳过名单豁免
 *   → 历史语境评审（pr-review-close 开启时：AI 对照相关历史 issue 的结论评审本 PR，
 *      证据确凿 → 评审评论 + 关闭；证据不足 → 回落旧链路）
 *   → 治理（要点提炼 + canonical 关联）。
 *
 * 与 issue 治理的安全阀完全同构：维护者 PR 跳过治理（垃圾检测保留）、dry-run 只评论、
 * AI 失败放行不动作。历史语境评审是唯一新增的合法 PR 关闭路径，且默认关闭（pr_review_close=false）。
 *
 * @param {Object} octokit GitHub API客户端
 * @param {Object} openai OpenAI客户端
 * @param {Object} context GitHub上下文
 * @param {string} owner 仓库所有者
 * @param {string} repo 仓库名
 * @param {string} aiModel AI模型名
 * @param {Object} config 配置对象
 * @param {Array} labelsList 标签列表
 * @param {Array} blacklistUsers 黑名单用户列表
 * @param {Object} gov 治理参数（与 GOVERNANCE_DEFAULTS 合并）
 */
async function handleNewPR(octokit, openai, context, owner, repo, aiModel, config, labelsList, blacklistUsers, gov = {}) {
  gov = { ...GOVERNANCE_DEFAULTS, ...gov };
  gov.skipUsers = [...new Set([...(GOVERNANCE_DEFAULTS.SKIP_USERS || []), ...(gov.skipUsers || [])])];

  try {
    const pr = context.payload.pull_request;
    const prTitle = pr.title;
    const prAuthor = pr.user.login.toLowerCase();

    core.info(logMessage(config.logging.pr_check_start, { title: prTitle }));

    // bot 自环豁免：治理自身创建的 canonical 又触发 PR 治理的死循环防护
    if (gov.skipUsers.includes(prAuthor)) {
      core.warning(logMessage(config.logging.governance_pr_skip_user, { number: pr.number }));
      return;
    }

    // 检查用户是否在黑名单中
    if (blacklistUsers.includes(prAuthor)) {
      await handleBlacklistedPR(octokit, owner, repo, pr, config);
      return;
    }

    // 获取PR的文件变更
    const fileChanges = await analyzeFileChanges(octokit, owner, repo, pr, config);

    // 创建工作流服务实例
    const workflowService = new PrWorkflowService(octokit, openai, aiModel, config);

    // 进行分层检测（垃圾/标题规范/质量）—— 上游链路，唯一会关 PR 的路径
    const analysisResult = await workflowService.performLayeredDetection(pr, fileChanges);
    const decision = analysisResult.decision;

    if (decision === 'SPAM') {
      await handleSpamPR(octokit, owner, repo, pr, config);
      return;
    } else if (decision === 'MALICIOUS' || decision === 'TRIVIAL') {
      await handleLowQualityPR(octokit, owner, repo, pr, config, decision);
      return;
    }

    // INVALID_COMMIT（标题不规范）不关闭：这正是治理「规范化改写标题」要接手的场景，流入分类 + 治理。
    // 有效 PR（KEEP / UNCLEAR / INVALID_COMMIT）：分类打标。UNCLEAR 的 PR 上游不关闭，同样进入治理（PR 永不因治理被关）
    let classification = null;
    if (labelsList && labelsList.length > 0) {
      classification = await workflowService.classifyAndLabelPR(owner, repo, pr, labelsList, fileChanges);
    }

    // 维护者豁免：协作者 PR 跳过治理（垃圾检测 + 分类保留），不评论、不改标题、不改正文、不建 issue
    if (gov.maintainerExempt) {
      const isMaintainer = await isCollaborator(octokit, owner, repo, prAuthor);
      if (isMaintainer) {
        core.info(logMessage(config.logging.governance_pr_skip_maintainer, { number: pr.number }));
        return;
      }
    }

    // 历史语境评审层（可选，pr-review-close 开启时）：AI 对照相关历史 issue/PR 的关闭结论评审本 PR。
    // 返回 null（未触发/证据不足/回落）时继续走下方旧关联链路；已处理（含 dry-run）则直接结束。
    // F3：两段式开启时，评审与治理共享同一份历史索引（单次拉取，C5/R5）；
    //      两段式关闭时 ctx=null，各服务行为与原先完全一致（byte-identical）。
    let sharedCtx = null;
    if (gov.enableTwoStage) {
      const HistoryContextService = require('../services/historyContextService');
      const historyContext = new HistoryContextService(octokit, config, gov);
      const index = await historyContext.buildIndex(owner, repo);
      sharedCtx = { historyContext, index };
    }

    if (gov.prReviewClose) {
      const reviewService = new PrReviewService(openai, aiModel, config, gov);
      const reviewResult = await reviewService.review(octokit, owner, repo, pr, fileChanges, sharedCtx);
      if (reviewResult) {
        return;
      }
    }

    // 治理层：要点提炼 + canonical 关联（永不关闭 PR）
    const governanceService = new PrGovernanceService(openai, aiModel, config, gov);
    await governanceService.govern(octokit, owner, repo, pr, classification, sharedCtx);

  } catch (error) {
    core.error(logMessage(config.logging.pr_process_error, { error: error.message }));

    if (isContentFilterError(error)) {
      const pr = context.payload.pull_request;
      core.warning(logMessage(config.logging.ai_content_filtered, { number: pr.number }));
      await closePRWithType(
        octokit,
        owner,
        repo,
        pr,
        config,
        'pr_content_filtered',
        'pr_content_filtered_log',
        false // 不锁定：回应承诺「编辑内容后重新提交」，与 issue 路径对齐（C4）
      );
      return;
    }

    // 安全阀：任何 PR 治理失败都不误关 PR —— 仅评论说明并放行
    await failOpenComment(octokit, owner, repo, context.payload.pull_request, config);
    core.error(config.logging.governance_pr_ai_fallback_log);
  }
}

/**
 * 通用PR关闭处理函数
 * @param {Object} octokit GitHub API客户端
 * @param {string} owner 仓库所有者
 * @param {string} repo 仓库名
 * @param {Object} pr PR对象
 * @param {Object} config 配置对象
 * @param {string} responseKey 响应消息键名
 * @param {string} logKey 日志消息键名
 * @param {boolean} shouldLock 是否锁定（C4：内容过滤与 TRIVIAL 不锁，保留「编辑后重提」出路；
 *   SPAM/MALICIOUS/黑名单维持锁定 —— 防御性决策，见 conflicts C4 / R4）
 */
async function closePRWithType(octokit, owner, repo, pr, config, responseKey, logKey, shouldLock = true) {
  await closePR(
    octokit,
    owner,
    repo,
    pr.number,
    config.responses[responseKey],
    config,
    shouldLock
  );

  core.info(logMessage(config.logging[logKey], { number: pr.number }));
}

/**
 * 处理低质量PR（恶意或无意义）
 */
async function handleLowQualityPR(octokit, owner, repo, pr, config, reason) {
  const responseMap = {
    'MALICIOUS': 'pr_malicious',
    'TRIVIAL': 'pr_trivial'
  };

  const logMap = {
    'MALICIOUS': 'pr_malicious_log',
    'TRIVIAL': 'pr_trivial_log'
  };

  const responseKey = responseMap[reason] || 'pr_closed';
  const logKey = logMap[reason] || 'pr_closed_log';
  // TRIVIAL 不锁定：pr_trivial 回应要求作者「确保提供有意义的改进」后重提（C4）；
  // MALICIOUS 维持锁定（防御性）。
  const shouldLock = reason !== 'TRIVIAL';

  await closePRWithType(octokit, owner, repo, pr, config, responseKey, logKey, shouldLock);
}

/**
 * 处理黑名单用户的PR
 */
async function handleBlacklistedPR(octokit, owner, repo, pr, config) {
  await closePRWithType(octokit, owner, repo, pr, config, 'pr_closed', 'pr_closed_log');
}

/**
 * 处理垃圾PR
 */
async function handleSpamPR(octokit, owner, repo, pr, config) {
  await closePRWithType(octokit, owner, repo, pr, config, 'pr_closed', 'pr_closed_log');
}

/**
 * 判断作者是否为仓库协作者（维护者豁免用）。
 * 查不到 / 非协作者都返回 false（宁可多治理，也不因权限查询失败而跳过真正需要治理的 PR）。
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
async function failOpenComment(octokit, owner, repo, pr, config) {
  const body = config.responses.governance_ai_fallback_comment;
  try {
    await addComment(
      octokit,
      owner,
      repo,
      pr.number,
      body,
      config.logging.governance_comment_failed
    );
  } catch (commentError) {
    core.warning(logMessage(config.logging.governance_comment_failed, { error: commentError.message }));
  }
}

/**
 * 分析PR的文件变更
 * @param {Object} octokit GitHub API客户端
 * @param {string} owner 仓库所有者
 * @param {string} repo 仓库名
 * @param {Object} pr PR对象
 * @param {Object} config 配置对象
 * @returns {Promise<string>} 文件变更描述
 */
async function analyzeFileChanges(octokit, owner, repo, pr, config) {
  if (!config.ai_settings.analyze_file_changes) {
    core.info(config.logging.file_analysis_disabled_info);
    return config.logging.file_analysis_disabled;
  }

  try {
    const filesResponse = await handleApiCall(
      () => octokit.rest.pulls.listFiles({
        owner,
        repo,
        pull_number: pr.number
      }),
      config.logging.pr_files_fetch_failed
    );

    if (filesResponse.data && filesResponse.data.length > 0) {
      const maxFiles = config.ai_settings.max_files_to_analyze || 5;
      const filesToAnalyze = filesResponse.data.slice(0, maxFiles);

      const fileChanges = filesToAnalyze.map(file => {
        let changeInfo = `${file.filename}(${file.status},+${file.additions}/-${file.deletions})`;

        if (file.patch) {
          const patchLines = file.patch.split('\n');
          const maxPatchLines = config.ai_settings.max_patch_lines_per_file || 5;
          const limitedPatch = patchLines
            .filter(line => line.startsWith('+') || line.startsWith('-'))
            .slice(0, maxPatchLines)
            .join('\n');

          if (limitedPatch.trim()) {
            changeInfo += `\n${limitedPatch}`;
            if (patchLines.filter(line => line.startsWith('+') || line.startsWith('-')).length > maxPatchLines) {
              changeInfo += '\n...';
            }
          }
        }

        return changeInfo;
      }).join('\n---\n');

      let result = fileChanges;
      if (filesResponse.data.length > maxFiles) {
        result += '\n' + logMessage(config.logging.file_changes_truncated, {
          total: filesResponse.data.length,
          shown: maxFiles
        });
      }

      core.info(logMessage(config.logging.file_changes_count, { count: filesResponse.data.length }));
      return result;
    } else {
      return config.logging.no_file_changes;
    }
  } catch (error) {
    core.warning(logMessage(config.logging.file_changes_error, { error: error.message }));
    return config.logging.file_changes_unavailable;
  }
}

module.exports = {
  handleNewPR,
  isCollaborator
};