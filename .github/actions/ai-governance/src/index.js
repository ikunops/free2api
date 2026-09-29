const core = require('@actions/core');
const OpenAI = require('openai');

const { loadConfig, parseInputs } = require('./utils/config');
const { logMessage } = require('./utils/helpers');
const { handleNewIssue } = require('./handlers/issueHandler');
const { handleNewPR } = require('./handlers/prHandler');
const { GOVERNANCE_DEFAULTS } = require('./utils/constants');

/**
 * 主程序入口
 */
async function run() {
  try {
    // 加载配置文件
    const baseConfig = loadConfig();

    // 解析输入参数（含治理层新增参数）
    const {
      token,
      aiModel,
      labelsList,
      blacklistUsers,
      analyzeFileChanges,
      analysisDepth,
      maxFilesToAnalyze,
      maxPatchLinesPerFile,
      customBaseUrl,
      customApiKey,
      governanceToken,
      prReviewClose,
      maxRelatedIssues,
      relatedCommentsPerIssue,
      relatedBodyTruncate,
      canonicalLabel,
      duplicateLabel,
      skipUsers,
      dryRun,
      maintainerExempt,
      enablePrGovernance,
      maxCanonicalIndex,
      canonicalBodyTruncate,
      // 统一历史语境层（F1）与两段式流水线（F2）
      maxHistoryIndex,
      enableTwoStage,
      maxScreenedCandidates,
      screeningModel,
      config
    } = parseInputs(baseConfig);

    // 初始化GitHub客户端（@actions/github v9 是纯 ESM 包，exports 无 require 条件，
    // CJS 侧必须走动态 import 加载——顶层 require 会抛 ERR_PACKAGE_PATH_NOT_EXPORTED）
    const github = await import('@actions/github');
    const octokit = github.getOctokit(token);
    const context = github.context;

    // 治理身份令牌：写操作改用该令牌的客户端 —— REST 写入的作者头像/账号由令牌身份决定。
    // 接 workflow 的 claude-identity 换票输出（runner OIDC 向 Anthropic 换来的 Claude App
    // installation token）后，治理评论/关闭将以 claude[bot] 署名。
    // 无该令牌时与原来完全一致（github-actions[bot]），纯增量、不破坏既有行为。
    let governanceOctokit = null;
    if (governanceToken) {
      governanceOctokit = github.getOctokit(governanceToken);
      core.info(config.logging.governance_identity_enabled);
    }

    // 确定使用的API配置
    // 注意：AI 鉴权优先用 github-token —— GitHub Models 依赖 workflow 的 models:read 权限，
    // 治理令牌（installation token）不一定具备，两者职责分离。
    const apiBaseUrl = customBaseUrl || config.defaults.api_base_url;
    const apiKey = customApiKey || token;

    // 初始化OpenAI客户端
    const openai = new OpenAI({
      baseURL: apiBaseUrl,
      apiKey: apiKey
    });

    // 输出配置信息
    if (customBaseUrl) {
      core.info(config.logging.using_custom_api);
    } else {
      core.info(config.logging.using_github_models);
    }
    core.info(logMessage(config.logging.using_ai_model, { model: aiModel }));
    core.info(config.logging.config_info);
    core.info(logMessage(config.logging.analysis_depth_info, { analyze_changes: analyzeFileChanges }));
    core.info(logMessage(config.logging.analysis_depth_details, {
      depth: analysisDepth,
      files: maxFilesToAnalyze,
      lines: maxPatchLinesPerFile
    }));

    // 获取仓库信息
    const { owner, repo } = context.repo;

    // 治理参数打包
    const gov = {
      canonicalLabel,
      duplicateLabel,
      skipUsers,
      dryRun,
      maintainerExempt,
      enablePrGovernance,
      maxCanonicalIndex,
      canonicalBodyTruncate,
      // PR 历史语境评审（新增）
      prReviewClose,
      maxRelatedIssues,
      relatedCommentsPerIssue,
      relatedBodyTruncate,
      // 统一历史语境层（F1）与两段式流水线（F2）
      maxHistoryIndex,
      enableTwoStage,
      maxScreenedCandidates,
      screeningModel,
      governanceToken
    };

    // 根据事件类型处理
    if (context.eventName === 'issues' && context.payload.action === 'opened') {
      await handleNewIssue(governanceOctokit || octokit, openai, context, owner, repo, aiModel, config, labelsList, blacklistUsers, gov);
    } else if ((context.eventName === 'pull_request_target') && context.payload.action === 'opened') {
      if (enablePrGovernance) {
        // PR 治理：上游垃圾检测（唯一会关 PR 的路径）+ 要点提炼 + canonical 关联（永不关闭合法 PR）
        // + 可选的历史语境评审（pr-review-close，见 prReviewService）
        await handleNewPR(governanceOctokit || octokit, openai, context, owner, repo, aiModel, config, labelsList, blacklistUsers, gov);
      } else {
        core.info('PR 治理默认关闭（enable-pr-governance=false），跳过');
      }
    } else {
      core.info(config.logging.event_no_match);
    }

  } catch (error) {
    core.setFailed(error.message);
  }
}

if (require.main === module) {
  run();
}

module.exports = { run, GOVERNANCE_DEFAULTS };