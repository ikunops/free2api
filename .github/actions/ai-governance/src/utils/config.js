const core = require('@actions/core');
const fs = require('fs');
const path = require('path');

const SUPPORTED_LANGUAGES = new Set(['en', 'zh-cn']);
const SUPPORTED_AI_API_TYPES = new Set(['chat-completions', 'responses']);

function normalizeLanguage(language) {
  return language?.trim().toLowerCase() === 'zh-cn' ? 'zh-CN' : 'en';
}

function applyLocale(config, requestedLanguage) {
  const language = normalizeLanguage(requestedLanguage);
  const localePath = path.join(__dirname, '..', '..', 'locales', `${language}.json`);
  const locale = JSON.parse(fs.readFileSync(localePath, 'utf8'));

  config.responses = { ...config.responses, ...locale.responses };
  config.locale = locale;
  config.defaults.language = language;

  return language;
}

/**
 * 验证配置完整性
 * @param {Object} config 配置对象
 * @throws {Error} 配置不完整时抛出错误
 */
function validateConfig(config) {
  const requiredSections = ['prompts', 'responses', 'logging', 'ai_settings', 'defaults'];
  const requiredPrompts = ['spam_detection', 'readme_coverage_check', 'content_quality_check', 'pr_spam_detection'];
  
  // 检查主要配置段
  for (const section of requiredSections) {
    if (!config[section]) {
      throw new Error(`配置文件缺少必需的段落: ${section}`);
    }
  }
  
  // 检查关键提示词
  for (const prompt of requiredPrompts) {
    if (!config.prompts[prompt]) {
      throw new Error(`配置文件缺少必需的提示词: ${prompt}`);
    }
  }
  
  // 检查AI设置
  if (typeof config.ai_settings.max_tokens !== 'number' || config.ai_settings.max_tokens <= 0) {
    throw new Error('配置文件中的 max_tokens 必须是正整数');
  }
  
  if (typeof config.ai_settings.temperature !== 'number' || config.ai_settings.temperature < 0 || config.ai_settings.temperature > 2) {
    throw new Error('配置文件中的 temperature 必须是 0-2 之间的数字');
  }
  
  core.info('✅ 配置文件验证通过');
}

/**
 * 读取配置文件
 * @returns {Object} 配置对象
 */
function loadConfig() {
  try {
    const configPath = path.join(__dirname, '..', '..', 'config.json');
    const configContent = fs.readFileSync(configPath, 'utf8');
    const config = JSON.parse(configContent);
    
    // 验证配置完整性
    validateConfig(config);
    
    return config;
  } catch (error) {
    core.error('无法读取配置文件: ' + error.message);
    throw error;
  }
}

/**
 * 数值输入解析：NaN 守卫（C17/FIX-E）。
 * 此前 parseInt('abc', 10) → NaN 直接传播进 slice(0, NaN) = 空列表，
 * 非数值输入会静默禁用整条特性。现在解析失败回落 fallback。
 */
function parseIntInput(value, fallback) {
  const parsed = parseInt(value, 10);
  return Number.isFinite(parsed) ? parsed : fallback;
}

/**
 * 治理输入声明表（R15/C15.6）：一个治理旋钮此前要在四处手写映射
 * （action.yml / parseInputs / GOVERNANCE_DEFAULTS / workflows yml），新增旋钮极易漏接线。
 * 现在收拢为单张声明表，parseInputs 统一消费：
 *   input  —— action.yml 的 input 名（同时对应 INPUT_* 环境变量）
 *   env    —— 环境变量名（与 input 一致，列表仅作文档）
 *   key    —— config.defaults 的默认值键
 *   out    —— parseInputs 返回对象上的 camelCase 键
 *   type   —— 'boolean' | 'int' | 'string'
 */
const GOV_INPUTS = [
  { input: 'canonical-label', env: 'INPUT_CANONICAL_LABEL', key: 'canonical_label', out: 'canonicalLabel', type: 'string' },
  { input: 'duplicate-label', env: 'INPUT_DUPLICATE_LABEL', key: 'duplicate_label', out: 'duplicateLabel', type: 'string' },
  { input: 'dry-run', env: 'INPUT_DRY_RUN', key: 'dry_run', out: 'dryRun', type: 'boolean' },
  { input: 'maintainer-exempt', env: 'INPUT_MAINTAINER_EXEMPT', key: 'maintainer_exempt', out: 'maintainerExempt', type: 'boolean' },
  { input: 'enable-pr-governance', env: 'INPUT_ENABLE_PR_GOVERNANCE', key: 'enable_pr_governance', out: 'enablePrGovernance', type: 'boolean' },
  { input: 'max-canonical-index', env: 'INPUT_MAX_CANONICAL_INDEX', key: 'max_canonical_index', out: 'maxCanonicalIndex', type: 'int' },
  { input: 'canonical-body-truncate', env: 'INPUT_CANONICAL_BODY_TRUNCATE', key: 'canonical_body_truncate', out: 'canonicalBodyTruncate', type: 'int' },
  { input: 'pr-review-close', env: 'INPUT_PR_REVIEW_CLOSE', key: 'pr_review_close', out: 'prReviewClose', type: 'boolean' },
  { input: 'max-related-issues', env: 'INPUT_MAX_RELATED_ISSUES', key: 'max_related_issues', out: 'maxRelatedIssues', type: 'int' },
  { input: 'related-comments-per-issue', env: 'INPUT_RELATED_COMMENTS_PER_ISSUE', key: 'related_comments_per_issue', out: 'relatedCommentsPerIssue', type: 'int' },
  { input: 'related-body-truncate', env: 'INPUT_RELATED_BODY_TRUNCATE', key: 'related_body_truncate', out: 'relatedBodyTruncate', type: 'int' },
  { input: 'max-history-index', env: 'INPUT_MAX_HISTORY_INDEX', key: 'max_history_index', out: 'maxHistoryIndex', type: 'int' },
  { input: 'enable-two-stage', env: 'INPUT_ENABLE_TWO_STAGE', key: 'enable_two_stage', out: 'enableTwoStage', type: 'boolean' },
  { input: 'max-screened-candidates', env: 'INPUT_MAX_SCREENED_CANDIDATES', key: 'max_screened_candidates', out: 'maxScreenedCandidates', type: 'int' },
  { input: 'screening-model', env: 'INPUT_SCREENING_MODEL', key: 'screening_model', out: 'screeningModel', type: 'string' }
];

/**
 * 按 GOV_INPUTS 声明表解析治理输入：input → env → defaults 三级回落，
 * boolean/int 类型带 NaN/大小写守卫（FIX-E 语义保持不变）。
 */
function parseGovInputs(config) {
  const out = {};
  for (const spec of GOV_INPUTS) {
    const raw = core.getInput(spec.input) || process.env[spec.env] || '';
    if (raw !== '') {
      if (spec.type === 'boolean') {
        out[spec.out] = raw.toLowerCase() === 'true';
        continue;
      }
      if (spec.type === 'int') {
        out[spec.out] = parseIntInput(raw, config.defaults[spec.key]);
        continue;
      }
      out[spec.out] = raw;
      continue;
    }
    const fallback = config.defaults[spec.key];
    if (spec.type === 'boolean') {
      out[spec.out] = String(fallback).toLowerCase() === 'true';
    } else if (spec.type === 'int') {
      out[spec.out] = parseIntInput(String(fallback), fallback);
    } else {
      out[spec.out] = fallback !== undefined ? fallback : '';
    }
  }
  return out;
}

/**
 * 解析用户输入参数
 * @param {Object} config 基础配置对象
 * @returns {Object} 解析后的配置对象
 */
function parseInputs(config) {
  // 获取输入参数，使用配置文件中的默认值
  const token = core.getInput('github-token') || process.env.INPUT_GITHUB_TOKEN || process.env.GITHUB_TOKEN;
  const aiModel = core.getInput('ai-model') || process.env.INPUT_AI_MODEL || config.defaults.ai_model;
  const labelsInput = core.getInput('labels') || process.env.INPUT_LABELS || config.defaults.labels;
  const blacklistUsersInput = core.getInput('blacklist') || process.env.INPUT_BLACKLIST || '';
  const requestedLanguage = core.getInput('language') || process.env.INPUT_LANGUAGE || config.defaults.language;
  const language = applyLocale(config, requestedLanguage);

  if (!SUPPORTED_LANGUAGES.has(requestedLanguage.trim().toLowerCase())) {
    core.warning(`Unsupported language "${requestedLanguage}"; falling back to English.`);
  }
  
  // 获取自定义AI配置参数
  const customBaseUrl = core.getInput('ai-base-url') || process.env.INPUT_AI_BASE_URL || '';
  const customApiKey = core.getInput('ai-api-key') || process.env.INPUT_AI_API_KEY || '';
  const aiApiType = (core.getInput('ai-api-type') || process.env.INPUT_AI_API_TYPE || config.defaults.ai_api_type).trim().toLowerCase();

  if (!SUPPORTED_AI_API_TYPES.has(aiApiType)) {
    throw new Error(`Unsupported AI API type: ${aiApiType}`);
  }
  
  // 解析黑名单用户列表
  const blacklistUsers = blacklistUsersInput
    ? blacklistUsersInput.split(',').map(user => user.trim().toLowerCase()).filter(user => user.length > 0)
    : [];
  
  // 获取新的配置参数，用户设置则用用户设置的，未设置则使用config中的默认值
  const analyzeFileChanges = core.getInput('analyze-file-changes') || process.env.INPUT_ANALYZE_FILE_CHANGES
    ? (core.getInput('analyze-file-changes') || process.env.INPUT_ANALYZE_FILE_CHANGES).toLowerCase() === 'true'
    : config.ai_settings.analyze_file_changes;

  // 治理开关与参数：GOV_INPUTS 声明表统一解析（R15/C15.6，替代四处分散的手写映射）
  const govInputs = parseGovInputs(config);
  const {
    canonicalLabel,
    duplicateLabel,
    dryRun,
    maintainerExempt,
    enablePrGovernance,
    maxCanonicalIndex,
    canonicalBodyTruncate,
    prReviewClose,
    maxRelatedIssues,
    relatedCommentsPerIssue,
    relatedBodyTruncate,
    maxHistoryIndex,
    enableTwoStage,
    maxScreenedCandidates,
    screeningModel
  } = govInputs;

  const skipUsersInput = core.getInput('skip-users') || process.env.INPUT_SKIP_USERS || '';
  const skipUsers = skipUsersInput
    ? skipUsersInput.split(',').map(u => u.trim().toLowerCase()).filter(u => u.length > 0)
    : [];

  // 治理身份令牌：传入后所有 GitHub 写操作（评论/关闭/打标/建 issue）以该令牌
  // 对应的账号身份发布。workflow 侧由 claude-identity 换票步骤提供（claude[bot]），
  // 缺省回落 github-token（github-actions[bot]）。
  // 注意：走 GitHub Models 时 AI 鉴权仍优先用 github-token（依赖 models:read），不强制要求本令牌具备。
  const governanceToken = core.getInput('governance-token') || process.env.INPUT_GOVERNANCE_TOKEN || '';

  // 解析分析深度参数，使用配置文件中的设置
  const analysisDepth = core.getInput('max-analysis-depth') || process.env.INPUT_MAX_ANALYSIS_DEPTH || config.defaults.analysis_depth;
  
  // 从配置文件获取分析深度设置
  const depthConfig = config.analysis_depths[analysisDepth.toLowerCase()] || config.analysis_depths.normal;
  const maxFilesToAnalyze = depthConfig.max_files;
  const maxPatchLinesPerFile = depthConfig.max_lines;
  
  // 更新配置对象
  config.ai_settings.analyze_file_changes = analyzeFileChanges;
  config.ai_settings.max_files_to_analyze = maxFilesToAnalyze;
  config.ai_settings.max_patch_lines_per_file = maxPatchLinesPerFile;
  config.ai_settings.api_type = aiApiType;
  
  // 解析标签列表
  const labelsList = labelsInput.split(',').map(label => label.trim()).filter(label => label.length > 0);

  return {
    token,
    aiModel,
    labelsList,
    blacklistUsers,
    language,
    analyzeFileChanges,
    analysisDepth,
    maxFilesToAnalyze,
    maxPatchLinesPerFile,
    customBaseUrl,
    customApiKey,
    aiApiType,
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
    maxHistoryIndex,
    enableTwoStage,
    maxScreenedCandidates,
    screeningModel,
    config
  };
}

module.exports = {
  loadConfig,
  parseInputs,
  validateConfig,
  applyLocale,
  normalizeLanguage,
  GOV_INPUTS,
  parseIntInput
};
