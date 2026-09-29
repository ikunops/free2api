const core = require('@actions/core');
const { logMessage, isValidCommitTitle } = require('../utils/helpers');
const { callAI } = require('./ai');
const { GOVERNANCE_DECISIONS, GOVERNANCE_DEFAULTS } = require('../utils/constants');
const IssueGovernanceService = require('./issueGovernanceService');
const githubOps = require('./github');

/**
 * PR 正文顶部的幂等锚点（HTML 注释）。追加关联块前检测是否已存在，避免重跑叠加。
 * 也用于测试断言「锚点真实存在且被检测」。
 */
const PR_LINK_ANCHOR = '<!-- ai-governance:linked -->';

/**
 * PR 治理服务 —— 与 issue 治理同体系的「整理 + 关联」，而不是「归并关闭」。
 *
 * 与 issue 治理的本质区别：
 *   issue 是无主诉求，治理目标是「关掉写得差的，翻译成 canonical」；
 *   PR 是代码贡献，必须保留 —— 治理目标是把它的元信息整理规范，并挂到对应 canonical 主题下。
 *
 * 因此 PR 治理链路永远不关闭 PR（唯一关闭路径仍在上游垃圾/恶意/trivial 检测）：
 *   - 复用 IssueGovernanceService 的 extractKeyPoints / matchCanonical / draftCanonical（提炼 + 匹配 + 起草，不复制粘贴）；
 *   - 匹配成功：评论（要点 + 关联说明）+ 规范化标题（若已规范则不改）+ 正文顶部追加关联块（幂等锚点）+
 *     在 canonical 追加 PR 关联记录；
 *   - 无匹配：起草并创建新 canonical（打 canonical + 分类标签）→ 评论 + 正文顶部追加 + canonical 附加记录，
 *     PR 正文关联块用 `Closes #N` 语义（PR 合并即关闭 canonical）；
 *   - 维护者豁免 / dry-run / AI 失败降级与 issue 治理同构；AI 失败一律放行不动作。
 *
 * 是否改写 PR 标题复用上游 pr_commit_check 的 VALID 口径：deterministic 校验
 * （helpers.isValidCommitTitle）通过即不改写，否则交 governance_pr_title 起草新标题。
 */
class PrGovernanceService {
  /**
   * @param {Object} openai OpenAI 客户端
   * @param {string} aiModel 模型名
   * @param {Object} config 合并后的配置
   * @param {Object} gov 治理参数（与 issue 治理共享 GOVERNANCE_DEFAULTS）
   * @param {Object} ops GitHub 写操作集合（默认 src/services/github.js）
   */
  constructor(openai, aiModel, config, gov = {}, ops = githubOps) {
    this.openai = openai;
    this.aiModel = aiModel;
    this.config = config;
    this.gov = { ...GOVERNANCE_DEFAULTS, ...gov };
    this.ops = ops;
    // 复用 issue 治理的提炼 / 匹配 / 起草能力（同一份实现，不做 PR 副本）
    this.issueGov = new IssueGovernanceService(openai, aiModel, config, this.gov, ops);
  }

  /**
   * 主流程：提炼 → 匹配 canonical → 路由（关联既有主题 / 开新主题），全程不关闭 PR。
   * @param {Object} octokit
   * @param {string} owner
   * @param {string} repo
   * @param {Object} pr 结构含 number/title/body/user.login
   * @param {string} classification PR 分类标签（可空；新主题建 issue 时随 canonical 一起打）
   * @param {Object|null} ctx 共享历史语境（F3）：{ historyContext, index }，两段式开启时由 handler 单次构建
   */
  async govern(octokit, owner, repo, pr, classification = null, ctx = null) {
    const { number } = pr;
    core.info(logMessage(this.config.logging.governance_pr_start, { number }));
    if (this.gov.dryRun) {
      core.info(logMessage(this.config.logging.governance_dry_run));
    }

    // 1. 要点提炼（复用 issue 治理服务；AI 失败抛错 → handler 兜底放行）
    const keyPoints = await this.issueGov.extractKeyPoints(pr);
    const summary = keyPoints.summary;

    // 2. 归并语料：两段式开启 → 共享索引筛 canonical（F2/F3，单次拉取 C5/R5）；关闭 → 旧 listCanonicalIssues
    const canonicalList = await this.issueGov.obtainCanonicalCorpus(octokit, owner, repo, ctx, pr);

    // 3. 归并匹配（复用 issue 治理的 matchCanonical；无索引直接视为新主题）
    let match = { decision: GOVERNANCE_DECISIONS.NEW_TOPIC };
    if (canonicalList.length > 0) {
      match = await this.issueGov.matchCanonical(pr, canonicalList);
    }
    core.info(logMessage(this.config.logging.governance_pr_match_result, {
      result: match.decision + (match.canonicalNumber ? `(#${match.canonicalNumber})` : '')
    }));

    // 4. 路由：匹配 → 关联既有主题；UNCERTAIN → 放行仅评论（与 issue 治理同构，不误建 canonical）；无匹配 → 开新主题
    if (match.decision === GOVERNANCE_DECISIONS.DUPLICATE) {
      return await this.routeLinkExisting(octokit, owner, repo, pr, keyPoints, summary, match.canonicalNumber, classification);
    }
    if (match.decision === GOVERNANCE_DECISIONS.UNCERTAIN) {
      return await this.routeUncertain(octokit, owner, repo, pr, summary);
    }
    return await this.routeNewTopic(octokit, owner, repo, pr, keyPoints, summary, classification);
  }

  /**
   * UNCERTAIN 放行：与 issue 治理同构 —— 证据不足时不建 canonical、不改标题、不改正文，仅评论说明。
   */
  async routeUncertain(octokit, owner, repo, pr, summary) {
    if (this.gov.dryRun) {
      await this.postDryRun(octokit, owner, repo, pr, summary);
      return { decision: GOVERNANCE_DECISIONS.UNCERTAIN, dryRun: true };
    }
    const comment = this.render('governance_pr_uncertain_comment', { summary });
    await this.safeComment(octokit, owner, repo, pr.number, comment);
    core.info(logMessage(this.config.logging.governance_pr_uncertain_log, { number: pr.number }));
    return { decision: GOVERNANCE_DECISIONS.UNCERTAIN, passed: true };
  }

  /**
   * 匹配成功：评论 + 规范化标题 + 正文顶部追加「Related to #N」关联块 + canonical 附加关联记录。
   */
  async routeLinkExisting(octokit, owner, repo, pr, keyPoints, summary, canonicalNumber) {
    const { number } = pr;
    const linkLine = this.render('governance_pr_match_link_line', { canonical: canonicalNumber });
    const comment = this.render('governance_pr_link_comment', { summary, link_line: linkLine });

    if (this.gov.dryRun) {
      await this.postDryRun(octokit, owner, repo, pr, summary, canonicalNumber);
      return { decision: GOVERNANCE_DECISIONS.DUPLICATE, canonicalNumber, dryRun: true };
    }

    // 先评论（评论失败不阻断后续，safe* 由本服务统一截获）
    await this.safeComment(octokit, owner, repo, number, comment);

    // 标题规范化（只改不规范标题）
    const normalizedTitle = await this.resolveTitle(pr, keyPoints);

    // 正文顶部追加（幂等锚点）
    const appendedBody = this.appendLinkBlock(
      number,
      pr.body,
      summary,
      this.renderRelation(canonicalNumber, false)
    );

    await this.applyPrUpdates(octokit, owner, repo, pr, normalizedTitle, appendedBody);

    // 在 canonical 追加关联记录（与 issue 归并记录同构）
    await this.safeRecordLink(octokit, owner, repo, canonicalNumber, pr, summary);

    core.info(logMessage(this.config.logging.governance_pr_linked, { number, canonical: canonicalNumber }));
    return { decision: GOVERNANCE_DECISIONS.DUPLICATE, canonicalNumber, linked: true };
  }

  /**
   * 无匹配：起草并创建新 canonical → 评论 + 规范化标题 + 正文顶部追加「Closes #N」关联块。
   */
  async routeNewTopic(octokit, owner, repo, pr, keyPoints, summary, classification) {
    const { number } = pr;
    const labels = [this.gov.canonicalLabel];
    if (classification) {
      labels.push(classification);
    }

    if (this.gov.dryRun) {
      await this.postDryRun(octokit, owner, repo, pr, summary, undefined, labels);
      return { decision: GOVERNANCE_DECISIONS.NEW_TOPIC, dryRun: true };
    }

    // 实际创建前才起草（省一次 AI 调用）
    const draft = await this.issueGov.draftCanonical(pr, keyPoints, classification || '');

    let canonicalNumber;
    try {
      const created = await this.ops.createIssue(octokit, owner, repo, draft.title, draft.body, labels);
      canonicalNumber = created.data ? created.data.number : null;
    } catch (error) {
      core.error(logMessage(this.config.logging.governance_create_canonical_failed, { error: error.message }));
      throw error; // handler 兜底：PR 保持原样，不关不动作
    }

    if (!canonicalNumber) {
      throw new Error('创建 canonical 后未拿到编号，中止以便重试');
    }
    core.info(logMessage(this.config.logging.governance_canonical_created, { number: canonicalNumber }));

    // 评论 + 标题规范化 + 正文顶部追加（Closes #N：PR 合并即关闭 canonical）
    const linkLine = this.render('governance_pr_new_link_line', { canonical: canonicalNumber });
    const comment = this.render('governance_pr_link_comment', { summary, link_line: linkLine });
    await this.safeComment(octokit, owner, repo, number, comment);

    const normalizedTitle = await this.resolveTitle(pr, keyPoints);
    const appendedBody = await this.appendLinkBlock(
      pr.number,
      pr.body,
      summary,
      this.renderRelation(canonicalNumber, true)
    );
    await this.applyPrUpdates(octokit, owner, repo, pr, normalizedTitle, appendedBody);

    // canonical 附加 PR 关联记录
    await this.safeRecordLink(octokit, owner, repo, canonicalNumber, pr, summary);

    core.info(logMessage(this.config.logging.governance_pr_new_topic, { number }));
    return { decision: GOVERNANCE_DECISIONS.NEW_TOPIC, canonicalNumber, created: true };
  }

  /**
   * 决定 PR 标题是否改写：已规范不乱动（复用上游 pr_commit_check 的 VALID 口径），
   * 不规范则交 AI 起一个 Conventional Commits 标题；若 AI 失败则回退到原标题，不阻断治理。
   */
  async resolveTitle(pr, keyPoints) {
    const original = (pr.title || '').trim();
    if (isValidCommitTitle(original)) {
      core.info(logMessage(this.config.logging.governance_pr_title_ok, { number: pr.number }));
      return original;
    }
    const request = {
      instructions: this.config.prompts.governance_pr_title,
      input: JSON.stringify({
        title: original,
        body: (pr.body || '').trim(),
        key_points: keyPoints
      })
    };
    try {
      const newTitle = (await callAI(this.openai, this.aiModel, request, this.config, 'PR 标题规范化', false)).trim();
      if (newTitle && newTitle !== original) {
        core.info(logMessage(this.config.logging.governance_pr_title_rewrite, { number: pr.number, title: newTitle }));
        return newTitle;
      }
    } catch (error) {
      core.warning(logMessage(this.config.logging.governance_pr_rewrite_failed, { number: pr.number, error: error.message }));
    }
    return original;
  }

  /**
   * 生成关联块（含幂等锚点），等价时返回 null（未变更则不调用 update API）。
   */
  renderRelation(canonicalNumber, closes) {
    const keyword = closes ? 'Closes' : 'Related to';
    return `### 关联 canonical issue\n\n${keyword} #${canonicalNumber}\n`;
  }

  /**
   * 在正文顶部追加关联块；已含锚点则跳过（幂等）。返回 null 表示内容未变化，跳过 API。
   * @param {number} prNumber PR 编号（仅用于幂等跳过日志）
   * @param {string} body 原正文
   * @param {string} summary 要点摘要
   * @param {string} relationBlock 关联块（Closes / Related to 语义）
   */
  appendLinkBlock(prNumber, body, summary, relationBlock) {
    const existing = String(body || '');
    if (existing.includes(PR_LINK_ANCHOR)) {
      core.info(logMessage(this.config.logging.governance_pr_body_linked_skip, { number: prNumber }));
      return null;
    }
    const keyText = summary ? summary.replace(/\n/g, '\n> ') : '(未提炼出明确要点)';
    const block = [
      PR_LINK_ANCHOR,
      '',
      '> 本段由 Claude Code 自动维护，请勿手工删除上面这行锚点注释。',
      '',
      '- **要点**：',
      `> ${keyText}`,
      `- **关联时间**：${new Date().toISOString()}`,
      '',
      relationBlock.trim()
    ].join('\n');
    return `${block}\n\n---\n\n${existing}`.replace(/\n{3,}/g, '\n\n');
  }

  /**
   * 统一应用标题 / 正文更新。dry-run 下不更新。单个字段更新失败只留痕，绝不关闭 PR。
   */
  async applyPrUpdates(octokit, owner, repo, pr, normalizedTitle, appendedBody) {
    if (this.gov.dryRun) {
      return;
    }
    const originalTitle = (pr.title || '').trim();
    const originalBody = pr.body || '';

    if (normalizedTitle && normalizedTitle !== originalTitle) {
      try {
        await this.ops.updatePullRequest(octokit, owner, repo, pr.number, { title: normalizedTitle });
      } catch (error) {
        core.warning(logMessage(this.config.logging.governance_pr_update_failed, { number: pr.number, error: error.message }));
      }
    }

    if (appendedBody && appendedBody !== originalBody) {
      try {
        await this.ops.updatePullRequest(octokit, owner, repo, pr.number, { body: appendedBody });
      } catch (error) {
        core.warning(logMessage(this.config.logging.governance_pr_update_failed, { number: pr.number, error: error.message }));
      }
    }
  }

  /**
   * canonical 上追加 PR 关联记录（来源 PR、作者、要点），与 issue 归并记录同构。
   */
  async safeRecordLink(octokit, owner, repo, canonicalNumber, pr, summary) {
    if (this.gov.dryRun) {
      return;
    }
    const record = this.render('governance_pr_canonical_record', {
      source: pr.number,
      author: pr.user?.login || 'unknown',
      summary,
      timestamp: new Date().toISOString()
    });
    try {
      await this.ops.addComment(octokit, owner, repo, canonicalNumber, record, this.config.logging.governance_comment_failed);
    } catch (error) {
      core.warning(logMessage(this.config.logging.governance_record_failed, {
        number: canonicalNumber,
        error: error.message
      }));
    }
  }

  render(templateKey, params) {
    const template = this.config.responses[templateKey] || this.config.locale?.responses?.[templateKey] || '';
    return logMessage(template, params);
  }

  async safeComment(octokit, owner, repo, number, body) {
    try {
      await this.ops.addComment(octokit, owner, repo, number, body, this.config.logging.governance_comment_failed);
    } catch (error) {
      core.warning(logMessage(this.config.logging.governance_comment_failed, { error: error.message }));
    }
  }

  async postDryRun(octokit, owner, repo, pr, summary, canonicalNumber, labels = []) {
    const actionText = canonicalNumber
      ? `关联 canonical #${canonicalNumber}；规范化标题；正文顶部追加关联块`
      : `新建 canonical issue（并打标签 ${labels.join(',')}）；规范化标题；正文顶部追加关联块`;
    const intro = this.config.responses.governance_dry_run;
    const body = `${intro}\n\n**本应执行**：${actionText}`;
    await this.safeComment(octokit, owner, repo, pr.number, body);
  }
}

module.exports = PrGovernanceService;
module.exports.PR_LINK_ANCHOR = PR_LINK_ANCHOR;