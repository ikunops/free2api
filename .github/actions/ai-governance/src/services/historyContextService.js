const core = require('@actions/core');
const { logMessage } = require('../utils/helpers');
const { GOVERNANCE_DEFAULTS } = require('../utils/constants');
const githubOps = require('./github');

// 时间线事件白名单（与 prReviewService.buildContextPackage 的过滤口径一致）
const TIMELINE_EVENTS = ['closed', 'reopened', 'merged', 'referenced', 'cross-referenced', 'locked', 'unlocked'];

/**
 * 统一历史语境层（F1）——「取回并组织仓库历史 issue 与 PR 作为参考经验」的唯一入口。
 *
 * 设计（spec F1）：
 *   - buildIndex：紧凑索引（每条一行），双通道并集去重：
 *       a) search API 全量 issue+PR（不分状态，C10：历史 PR 也是参考经验）
 *       b) canonical 标签列表（保证入选，带 canonical 标记）
 *     单次治理运行只应调用一次，评审与关联层共享（C5）。
 *   - enrich：深补全 —— #189 的 fetchRelatedIssues 泛化到 issue 与 PR：
 *     正文（截断）+ 评论（每条截断 500，封顶）+ 时间线（白名单过滤）；
 *     PR 额外含 files_changed 摘要与提交列表。
 *     单条失败跳过（容忍语义），全失败返回 []。
 *
 * 安全阀：检索失败一律 fail-soft（空索引/空详情），调用方回落旧行为，绝不抛错升级。
 */
class HistoryContextService {
  /**
   * @param {Object} octokit GitHub API 客户端
   * @param {Object} config 合并后的配置
   * @param {Object} gov 治理参数（与 GOVERNANCE_DEFAULTS 合并）
   * @param {Object} ops GitHub 操作集合（默认 src/services/github.js，测试注入 mock）
   */
  constructor(octokit, config, gov = {}, ops = githubOps) {
    this.octokit = octokit;
    this.config = config;
    this.gov = { ...GOVERNANCE_DEFAULTS, ...gov };
    this.ops = ops;
  }

  /**
   * 构建紧凑历史索引。
   * 索引条目额外携带截断正文（body）—— 筛选阶段会剥离它（ScreeningService 只取
   * 紧凑字段），但归并匹配阶段用它保持与旧 listCanonicalIssues 提示词口径一致。
   * @returns {Promise<Array<HistoryIndexItem>}
   *   [{ number, kind: 'issue'|'pr', title, labels: string[], state: 'open'|'closed'|'merged',
   *      state_reason, closed_at, body }]
   */
  async buildIndex(owner, repo) {
    const candidates = new Map(); // `${kind}:${number}` → item
    const truncate = (text) => {
      const s = String(text || '');
      return s.length > this.gov.relatedBodyTruncate ? s.slice(0, this.gov.relatedBodyTruncate) : s;
    };

    // 通道 a：全量 issue+PR 检索（C10：历史 PR 必须进语料）
    let searchItems = [];
    try {
      searchItems = await this.ops.searchIssuesAndPRs(this.octokit, owner, repo, this.gov.maxHistoryIndex);
    } catch (error) {
      // fail-soft：检索失败回落 canonical-only 索引
      core.warning(logMessage(this.config.logging.history_index_search_failed, { error: error.message }));
    }
    searchItems.forEach(item => {
      candidates.set(`${item.kind}:${item.number}`, { ...item, body: truncate(item.body) });
    });

    // 通道 b：canonical 标签（治理一手结论，保证入选，标记 canonical）
    let canonicalItems = [];
    try {
      canonicalItems = await this.ops.listCanonicalIssues(
        this.octokit,
        owner,
        repo,
        this.gov.canonicalLabel,
        this.gov.maxHistoryIndex,
        this.gov.relatedBodyTruncate,
        true
      );
    } catch (error) {
      core.warning(logMessage(this.config.logging.history_index_canonical_failed, { error: error.message }));
    }
    canonicalItems.forEach(item => {
      // canonical 优先：覆盖 search 通道的同号条目（保留其 state 字段，R2）
      candidates.set(`issue:${item.number}`, {
        number: item.number,
        kind: 'issue',
        title: item.title,
        labels: ['canonical'],
        state: item.state || 'closed',
        state_reason: item.state_reason || null,
        closed_at: item.closed_at || null,
        body: truncate(item.body)
      });
    });

    // 上限截断（canonical 通道已在前面覆盖排序，直接截断即可）
    return [...candidates.values()].slice(0, this.gov.maxHistoryIndex);
  }

  /**
   * 深补全筛出的候选（issue 与 PR 通用）。
   * @param {Array<{number, kind}>} refs
   * @returns {Promise<Array<HistoryDetail>>}
   */
  async enrich(owner, repo, refs) {
    const details = [];
    for (const ref of refs || []) {
      try {
        const detail = await this.enrichOne(owner, repo, ref);
        if (detail) {
          details.push(detail);
        }
      } catch (error) {
        // 单条失败容忍：跳过该条继续（与 #189 enrichRelatedIssues 同语义）
        core.warning(logMessage(this.config.logging.history_enrich_failed, {
          number: ref.number,
          error: error.message
        }));
      }
    }
    return details;
  }

  async enrichOne(owner, repo, { number, kind }) {
    const [detail, comments, timeline] = await Promise.all([
      this.ops.getIssueDetail(this.octokit, owner, repo, number),
      this.ops.listIssueComments(this.octokit, owner, repo, number, this.gov.relatedCommentsPerIssue),
      this.ops.listIssueTimeline(this.octokit, owner, repo, number, 30)
    ]);

    const truncate = (text, limit) => {
      const s = String(text || '');
      return s.length > limit ? `${s.slice(0, limit)}…(截断)` : s;
    };

    const item = {
      number,
      kind: kind === 'pr' ? 'pr' : 'issue',
      title: detail.title,
      state: detail.state,
      state_reason: detail.state_reason,
      closed_at: detail.closed_at,
      body: truncate(detail.body, this.gov.relatedBodyTruncate),
      comments: (comments || []).map(c => ({
        author: c.author,
        body: truncate(c.body, 500),
        created_at: c.created_at
      })),
      timeline: (timeline || [])
        .filter(e => TIMELINE_EVENTS.includes(e.event))
        .map(e => ({ event: e.event, actor: e.actor, commit_id: e.commit_id }))
    };

    if (item.kind === 'pr') {
      const [filesSummary, commits] = await Promise.all([
        this.ops.listPRFilesSummary(this.octokit, owner, repo, number, {
          maxFiles: this.config.ai_settings.max_files_to_analyze || 5,
          maxPatchLines: this.config.ai_settings.max_patch_lines_per_file || 5
        }),
        this.ops.listPRCommits(this.octokit, owner, repo, number, 20)
      ]);
      item.files_changed = truncate(filesSummary.summary, this.gov.relatedBodyTruncate);
      item.commits = commits.map(c => ({ message: truncate(c.message, 200), author: c.author }));
    }

    return item;
  }
}

module.exports = HistoryContextService;
