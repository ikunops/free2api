const core = require('@actions/core');
const { logMessage } = require('../utils/helpers');
const { callAI, callAIStructured } = require('./ai');
const { analyzeIssueQuality } = require('./templateDetector');
const { GOVERNANCE_DECISIONS, GOVERNANCE_DEFAULTS } = require('../utils/constants');
const githubOps = require('./github');

const DUPLICATE_PATTERN = /^DUPLICATE\(#?(\d+)\)/i;

/**
 * 从多态「请求体」统一拆出 title/body，供 issue 与 PR 两条治理链路复用。
 * PR 治理传入 { number, title, body }，issue 治理传入标准 issue 结构（body 可空）。
 * @returns {{title: string, body: string}}
 */
function splitTitleBody(content) {
  return {
    title: (content.title || '').trim(),
    body: (content.body || '').trim()
  };
}

/**
 * Issue 治理服务 —— AI 驱动的「规范化 + 归并」。
 *
 * 在保留上游垃圾检测 / README 覆盖 / 内容分级的基础上，新增一条治理链路：
 *   要点提炼 → 规范判定 → canonical 归并匹配 → （归并 / 重开成 canonical / 放行）。
 *
 * 安全设计（见 DESIGN.md）：
 *   - 所有 AI 调用失败时抛错，由 handler 兜底「宁可漏判、不可误关」，只评论不关闭。
 *   - 写入顺序固定为「先评论、后关」，创建 canonical 失败时绝不关闭原 issue。
 *   - dry-run 模式只评论，不做任何关闭 / 创建 / 打标签动作。
 */
class IssueGovernanceService {
  /**
   * @param {Object} openai OpenAI 客户端
   * @param {string} aiModel 模型名
   * @param {Object} config 合并后的配置（含 prompts/responses/logging）
   * @param {Object} gov 治理参数（canonicalLabel / duplicateLabel / maxCanonicalIndex / dryRun / canonicalBodyTruncate）
   * @param {Object} ops GitHub 写操作集合（默认用 src/services/github.js，测试时注入 mock）
   */
  constructor(openai, aiModel, config, gov = {}, ops = githubOps) {
    this.openai = openai;
    this.aiModel = aiModel;
    this.config = config;
    this.gov = { ...GOVERNANCE_DEFAULTS, ...gov };
    this.ops = ops;
  }

  /**
   * 第一步：要点提炼（结构化输出：要点 + 要做的事）
   * @returns {Promise<{summary: string, key: string, todos: string[]}>}
   */
  async extractKeyPoints(issue) {
    core.info(logMessage(this.config.logging.governance_extract_start, { number: issue.number }));
    const { title, body } = splitTitleBody(issue);
    const request = {
      instructions: this.config.prompts.governance_extract,
      input: JSON.stringify({ title, body })
    };
    const result = await callAIStructured(
      this.openai,
      this.aiModel,
      request,
      this.config,
      '要点提炼',
      { '要点': '', '要做的事': [] }
    );
    if (!result) {
      throw new Error('要点提炼返回无法解析的内容');
    }
    const key = result['要点'];
    const todos = Array.isArray(result['要做的事']) ? result['要做的事'] : [];
    return { key, todos, summary: this.formatSummary(key, todos) };
  }

  /**
   * 第二步：规范判定。WELL_FORMED = 模板化 + 单主题 + 信息完整，可原地保留 / 提升为 canonical。
   * 两层判据：
   *   1) 确定性完整性检查（模板命中 + 有效段落数 + 标题/正文长度，全过则为 WELL_FORMED）；
   *   2) 否则交 AI 做单主题/信息量判读，AI 认为规范才 WELL_FORMED —— 宁可重开也不让乱 issue 洗白为 canonical。
   */
  async checkWellFormed(issue) {
    if (this.meetsDeterministicThreshold(issue)) {
      core.info(`Issue #${issue.number} 通过确定性完整性检查，判定为规范`);
      return 'WELL_FORMED';
    }
    const request = {
      instructions: this.config.prompts.governance_well_formed,
      input: JSON.stringify(splitTitleBody(issue))
    };
    const decision = await callAI(this.openai, this.aiModel, request, this.config, '规范判定');
    return decision === 'WELL_FORMED' ? 'WELL_FORMED' : 'NEEDS_NORMALIZE';
  }

  /**
   * 确定性完整性检查：复用上游 templateDetector 的质量分析，
   * 模板命中且有效段落数达到门槛 + 标题/正文长度过关 → 规范。
   */
  meetsDeterministicThreshold(issue) {
    const title = (issue.title || '').trim();
    const body = (issue.body || '').trim();
    if (title.length < this.gov.wellFormedMinTitleLen) {
      return false;
    }
    if (body.length < this.gov.wellFormedMinBodyLen) {
      return false;
    }
    const analysis = analyzeIssueQuality(issue.title, issue.body);
    const sections = analysis.contentInfo && analysis.contentInfo.validSections
      ? analysis.contentInfo.validSections
      : 0;
    return analysis.templateInfo.hasTemplate && sections >= this.gov.wellFormedMinSections;
  }

  /**
   * 第三步：归并匹配。返回 DUPLICATE(#N) / NEW_TOPIC / UNCERTAIN。
   * F2 防幻觉闸门：DUPLICATE(#N) 的 N 必须在候选语料里，否则降级 UNCERTAIN（宁可漏判）。
   */
  async matchCanonical(issue, canonicalList) {
    core.info(logMessage(this.config.logging.governance_match_start, { number: issue.number }));
    const { title, body } = splitTitleBody(issue);
    const request = {
      instructions: this.config.prompts.governance_merge_match,
      input: JSON.stringify({
        new_issue: { number: issue.number, title, body },
        canonical_index: canonicalList
      })
    };
    const raw = await callAI(this.openai, this.aiModel, request, this.config, '归并匹配');
    const match = (raw || '').trim();
    const dup = match.match(DUPLICATE_PATTERN);
    if (dup) {
      const canonicalNumber = parseInt(dup[1], 10);
      if (!canonicalList.some(c => c.number === canonicalNumber)) {
        core.warning(logMessage(this.config.logging.governance_match_candidate_rejected, { number: canonicalNumber }));
        return { decision: GOVERNANCE_DECISIONS.UNCERTAIN };
      }
      return { decision: GOVERNANCE_DECISIONS.DUPLICATE, canonicalNumber };
    }
    if (/^NEW_TOPIC/.test(match)) {
      return { decision: GOVERNANCE_DECISIONS.NEW_TOPIC };
    }
    return { decision: GOVERNANCE_DECISIONS.UNCERTAIN };
  }

  /**
   * 为规范 issue 生成 AI 评审评论：感谢 + 分析认可 + 实现方案 + 后续邀请 PR。
   * 输入中的 title/body 均为不可信数据，作者与编号由服务端注入，避免模板被伪造。
   * F3：可携带 relatedHistory（筛查出的相关历史 issue/PR，含结论摘要），
   * 「实现方案/后续」可引用真实历史结论；空数组时提示词要求不引用任何编号（防幻觉）。
   * @param {Object} issue
   * @param {Array} relatedHistory [{ number, kind, title, state, state_reason, conclusion_snippet }]
   * @returns {Promise<string|null>} AI 生成的评论文本，失败时返回 null 由调用方兜底
   */
  async draftWellFormedReview(issue, relatedHistory = []) {
    const { title, body } = splitTitleBody(issue);
    const request = {
      instructions: this.config.prompts.governance_well_formed_review,
      input: JSON.stringify({
        title,
        body,
        author: issue.user?.login || 'unknown',
        number: issue.number,
        related_history: (relatedHistory || []).slice(0, this.gov.maxScreenedCandidates)
      })
    };
    const raw = await callAI(this.openai, this.aiModel, request, this.config, '生成规范 issue 评审评论', false);
    return String(raw || '').trim() || null;
  }

  /**
   * 第四步（仅新主题非规范时）：起草规范化 canonical issue。
   */
  async draftCanonical(issue, keyPoints, classification) {
    const request = {
      instructions: this.config.prompts.governance_new_issue,
      input: JSON.stringify({
        number: issue.number,
        title: issue.title,
        body: issue.body || '',
        key_points: keyPoints
      })
    };
    const raw = await callAI(this.openai, this.aiModel, request, this.config, '起草规范化 issue', false);
    return this.splitCanonical(raw, issue.title, classification);
  }

  /**
   * 把 AI 草稿切成「标题 + 正文」，标题兜底补上 [Feature]/[Bug] 前缀。
   */
  splitCanonical(raw, fallbackTitle, classification) {
    const text = String(raw || '').trim();
    const lines = text.split('\n').map(l => l.trimEnd());
    let title = (lines[0] || '').replace(/^#+\s*/, '').trim() || fallbackTitle;
    if (!/^\[(Feature|Bug|Enhancement)\]/.test(title)) {
      const prefix = /bug|fix/i.test(classification || '') ? '[Bug] ' : '[Feature] ';
      title = prefix + title.replace(/^\[[^\]]*\]\s*/, '');
    }
    const body = lines.slice(1).join('\n').trim();
    return { title, body };
  }

  formatSummary(key, todos) {
    const lines = [`要点：${key || '(未提炼出明确要点)'}`];
    if (todos.length > 0) {
      lines.push('要做的事：');
      todos.forEach(item => lines.push(`- ${item}`));
    }
    return lines.join('\n');
  }

  /**
   * 主流程：提炼 + 规范判定 + 匹配 + 路由（并执行写入）。
   * @param {Object} octokit
   * @param {string} owner
   * @param {string} repo
   * @param {Object} issue 结构含 number/title/body/user.login
   * @param {string|null} classification 分类标签（bug/enhancement/...）
   * @param {Object|null} ctx 共享历史语境（F3）：{ historyContext, index }，两段式开启时由 handler 单次构建
   */
  async govern(octokit, owner, repo, issue, classification = null, ctx = null) {
    const { number } = issue;
    core.info(logMessage(this.config.logging.governance_start, { number }));
    if (this.gov.dryRun) {
      core.info(logMessage(this.config.logging.governance_dry_run));
    }

    // 1. 要点提炼（AI 失败在此抛错，由 handler 兜底放行）
    const keyPoints = await this.extractKeyPoints(issue);
    const summary = keyPoints.summary;

    // 2. 规范判定
    const wellFormed = await this.checkWellFormed(issue);

    // 3. 归并语料：两段式开启 → 共享历史索引筛 canonical（F2/F3）；关闭 → 直接拉 canonical 列表（旧行为）
    const canonicalList = await this.obtainCanonicalCorpus(octokit, owner, repo, ctx, issue);

    // 4. 归并匹配（无 canonical 索引时直接视为新主题，省一次 AI 调用；
    //    防幻觉闸门在 matchCanonical 内部：DUPLICATE(#N) 的 N 必须在语料里）
    let match = { decision: GOVERNANCE_DECISIONS.NEW_TOPIC };
    if (canonicalList.length > 0) {
      match = await this.matchCanonical(issue, canonicalList);
    }
    core.info(logMessage(this.config.logging.governance_match_result, {
      result: match.decision + (match.canonicalNumber ? `(#${match.canonicalNumber})` : '')
    }));

    // 5. 路由
    if (match.decision === GOVERNANCE_DECISIONS.DUPLICATE) {
      return await this.routeDuplicate(octokit, owner, repo, issue, summary, match.canonicalNumber);
    } else if (match.decision === GOVERNANCE_DECISIONS.UNCERTAIN) {
      return await this.routeUncertain(octokit, owner, repo, issue, summary);
    } else if (wellFormed === 'WELL_FORMED') {
      // 新主题 + 已规范：原地提升为 canonical，不重开。
      // F3：两段式开启时先筛查相关历史（共享索引，不重复拉取）供评审评论引用
      const relatedHistory = await this.obtainRelatedHistory(ctx, issue);
      return await this.routePromoteInPlace(octokit, owner, repo, issue, summary, classification, relatedHistory);
    } else {
      // 新主题 + 不规范：重开成规范 canonical
      return await this.routeNormalize(octokit, owner, repo, issue, summary, keyPoints, classification);
    }
  }

  /**
   * 为规范 issue 评审评论筛查相关历史（F3）：
   * 两段式开启且有共享索引 → 筛选 AI 在全量索引（issue+PR，C10）上选候选，
   * 返回紧凑形状 [{ number, kind, title, state, state_reason, conclusion_snippet }]（封顶 maxScreenedCandidates）；
   * 其余情形返回 []（评审评论不带历史引用，行为与原先一致）。
   */
  async obtainRelatedHistory(ctx, issue) {
    if (!this.gov.enableTwoStage || !ctx || !Array.isArray(ctx.index) || ctx.index.length === 0) {
      return [];
    }
    const ScreeningService = require('./screeningService');
    const screening = new ScreeningService(this.openai, this.aiModel, this.config, this.gov);
    try {
      const screened = await screening.screen(
        { kind: 'issue', number: issue.number, title: issue.title, body: issue.body },
        ctx.index
      );
      return screened
        .map(c => {
          const hit = ctx.index.find(i => i.kind === c.kind && i.number === c.number);
          if (!hit) {
            return null;
          }
          const conclusion = hit.state_reason
            ? `${hit.state} (${hit.state_reason})`
            : hit.state;
          return {
            number: hit.number,
            kind: hit.kind,
            title: hit.title,
            state: hit.state,
            state_reason: hit.state_reason,
            conclusion_snippet: `#${hit.number} ${hit.title} — ${conclusion}`
          };
        })
        .filter(Boolean);
    } catch (error) {
      core.warning(logMessage(this.config.logging.history_screening_failed, { error: error.message }));
      return [];
    }
  }

  /**
   * 归并语料获取（F2/F3）：
   *   - enableTwoStage 开启：从共享历史索引（ctx，C5 单次拉取）筛出 canonical-labeled 条目，
   *     先经筛选 AI 选候选（issue+PR 语料里 canonical 标记的），候选深补全后作为阶段二语料 ——
   *     阶段二看到候选的评论历史（R9：merge-match 语料含评论历史）；
   *   - 关闭：直接 listCanonicalIssues（与原先完全一致）。
   * 两条路径都失败容忍为空（→ 视为新主题）。
   */
  async obtainCanonicalCorpus(octokit, owner, repo, ctx, issue) {
    // 旧行为（flag off / 无 ctx）：直接拉 canonical 列表
    if (!this.gov.enableTwoStage || !ctx || !Array.isArray(ctx.index)) {
      let canonicalList = [];
      try {
        core.info(logMessage(this.config.logging.governance_fetch_canonical, { label: this.gov.canonicalLabel }));
        canonicalList = await this.ops.listCanonicalIssues(
          octokit,
          owner,
          repo,
          this.gov.canonicalLabel,
          this.gov.maxCanonicalIndex,
          this.gov.canonicalBodyTruncate,
          true
        );
        core.info(logMessage(this.config.logging.governance_canonical_count, { count: canonicalList.length }));
      } catch (error) {
        core.warning(logMessage(this.config.logging.governance_canonical_fetch_failed, { error: error.message }));
        canonicalList = [];
      }
      return canonicalList;
    }

    // 两段式：共享索引里筛 canonical 条目 → 筛选 AI 选候选 → 深补全（含评论历史，R9）
    const canonicalIndex = ctx.index.filter(item =>
      item.kind === 'issue' && (item.labels || []).includes(this.gov.canonicalLabel)
    );
    if (canonicalIndex.length === 0) {
      core.info(logMessage(this.config.logging.governance_canonical_count, { count: 0 }));
      return [];
    }

    const ScreeningService = require('./screeningService');
    const screening = new ScreeningService(this.openai, this.aiModel, this.config, this.gov);
    const screened = await screening.screen(
      { kind: 'issue', number: issue.number, title: issue.title, body: issue.body },
      canonicalIndex
    );

    if (screened.length === 0) {
      // 筛选为空 → 回落全量 canonical 索引（截断口径与旧行为一致），不做全量补全（省 API 调用）
      return canonicalIndex.slice(0, this.gov.maxCanonicalIndex).map(item => ({
        number: item.number,
        title: item.title,
        body: item.body || '',
        state: item.state,
        state_reason: item.state_reason,
        closed_at: item.closed_at
      }));
    }

    // 候选深补全：正文 + 评论 + 时间线（R9：阶段二语料含 canonical 评论历史）
    try {
      const enriched = await ctx.historyContext.enrich(owner, repo, screened.map(c => ({ number: c.number, kind: 'issue' })));
      return enriched.map(e => ({
        number: e.number,
        title: e.title,
        body: [e.body, ...(e.comments || []).map(c => `${c.author}: ${c.body}`)].join('\n\n---\n\n'),
        state: e.state,
        state_reason: e.state_reason,
        closed_at: e.closed_at
      }));
    } catch (error) {
      core.warning(logMessage(this.config.logging.governance_canonical_fetch_failed, { error: error.message }));
      return [];
    }
  }

  async routeDuplicate(octokit, owner, repo, issue, summary, canonicalNumber) {
    const { number } = issue;
    const mergeComment = this.render('governance_merge_comment', {
      summary,
      canonical: canonicalNumber
    });

    if (this.gov.dryRun) {
      await this.postDryRun(octokit, owner, repo, issue, mergeComment, `归并到 #${canonicalNumber}`);
      return { decision: GOVERNANCE_DECISIONS.DUPLICATE, canonicalNumber, dryRun: true };
    }

    // 顺序：打标签 → 评论 → 关闭 → 在 canonical 追加归并记录
    await this.safeAddLabels(octokit, owner, repo, number, [this.gov.duplicateLabel]);
    await this.safeComment(octokit, owner, repo, number, mergeComment);
    await this.safeClose(octokit, owner, repo, number);
    await this.safeRecordMerge(octokit, owner, repo, canonicalNumber, issue, summary);

    core.info(logMessage(this.config.logging.governance_duplicate_log, { number, canonical: canonicalNumber }));
    return { decision: GOVERNANCE_DECISIONS.DUPLICATE, canonicalNumber, closed: true };
  }

  async routeUncertain(octokit, owner, repo, issue, summary) {
    const comment = this.render('governance_uncertain_comment', { summary });
    if (this.gov.dryRun) {
      await this.postDryRun(octokit, owner, repo, issue, comment, '放行（UNCERTAIN）');
      return { decision: GOVERNANCE_DECISIONS.UNCERTAIN, dryRun: true };
    }
    // 宁可漏判不可误关：UNCERTAIN 一律放行，仅评论说明
    await this.safeComment(octokit, owner, repo, issue.number, comment);
    return { decision: GOVERNANCE_DECISIONS.UNCERTAIN, passed: true };
  }

  async routePromoteInPlace(octokit, owner, repo, issue, summary, classification, relatedHistory = []) {
    const { number } = issue;
    const labels = [this.gov.canonicalLabel];
    if (classification) {
      labels.push(classification);
    }
    const comment = await this.buildWellFormedComment(issue, summary, relatedHistory);

    if (this.gov.dryRun) {
      await this.postDryRun(octokit, owner, repo, issue, comment, `无需重开，仅打标 ${labels.join(',')}`);
      return { decision: 'WELL_FORMED', dryRun: true };
    }

    await this.safeAddLabels(octokit, owner, repo, number, labels);
    await this.safeComment(octokit, owner, repo, number, comment);

    core.info(logMessage(this.config.logging.governance_well_formed, { number }));
    return { decision: 'WELL_FORMED', promoted: true };
  }

  /**
   * 规范 issue 的评论内容：优先用 AI 生成「分析认可 + 实现方案 + 后续」的实质性评审；
   * AI 失败时回落到既有固定模板，保证流程永不中断（宁可漏判、不可误关精神）。
   * 两种路径都统一追加机器人操作日志行，用户仍能识别这是机器人评论。
   * F3：可携带 relatedHistory 供评审引用；草稿引用的每个 #N 都过确定性校验（防幻觉），
   * 校验失败回落固定模板（不发布含幻觉编号的公开评论）。
   */
  async buildWellFormedComment(issue, summary, relatedHistory = []) {
    try {
      const review = await this.draftWellFormedReview(issue, relatedHistory);
      if (review) {
        const validNumbers = new Set([issue.number, ...(relatedHistory || []).map(h => h.number)]);
        const cited = review.match(/#(\d+)/g) || [];
        const fabricated = cited.filter(m => !validNumbers.has(parseInt(m.slice(1), 10)));
        if (fabricated.length > 0) {
          // 引用闸门（F3）：草稿引用了历史集合之外的编号 → 整条草稿作废，回落固定模板
          core.warning(logMessage(this.config.logging.governance_citation_rejected, {
            number: issue.number,
            cited: fabricated.join(' ')
          }));
        } else {
          let text = this.withLogLine(this.withBotPrefix(review), 'governance_well_formed_comment');
          if (relatedHistory.length > 0) {
            // 有引用历史时服务端确定性追加一行指引（与 PR 评审评论同约定）
            text = `${text}\n\n${this.config.responses.governance_history_reference_note}`;
          }
          return text;
        }
      }
    } catch (error) {
      core.warning(logMessage(this.config.logging.governance_review_failed, {
        number: issue.number,
        error: error.message
      }));
    }
    return this.render('governance_well_formed_comment', { summary });
  }

  /**
   * 给 AI 生成的评论补上机器人操作日志行（与 render() 的尾部行为保持一致）。
   */
  withLogLine(text, action) {
    return `${text}\n\n${logMessage(this.config.responses.governance_log_prefix, { action })}`;
  }

  /**
   * 确保 AI 评审评论以 🤖 开头（与固定模板一致）：
   * 机器人身份标记由服务端确定性保证，不依赖模型自觉。
   */
  withBotPrefix(text) {
    const trimmed = String(text || '').trim();
    return trimmed.startsWith('🤖') ? trimmed : `🤖 ${trimmed}`;
  }

  async routeNormalize(octokit, owner, repo, issue, summary, keyPoints, classification) {
    const { number } = issue;
    const labels = [this.gov.canonicalLabel];
    if (classification) {
      labels.push(classification);
    }

    if (this.gov.dryRun) {
      await this.postDryRun(
        octokit,
        owner,
        repo,
        issue,
        this.render('governance_new_issue_comment', { summary, canonical: '（编号待实际创建后确定）' }),
        `创建规范化 canonical issue（[Feature]/[Bug] 前缀）并打标 ${labels.join(',')}`
      );
      return { decision: GOVERNANCE_DECISIONS.NEW_TOPIC, dryRun: true };
    }

    // 实际创建前才起草（省一次 AI 调用）
    const draft = await this.draftCanonical(issue, keyPoints, classification);

    // 先创建 canonical，成功后才允许关闭原 issue，避免留下「无归处」的关闭状态
    let canonicalNumber;
    try {
      const created = await this.ops.createIssue(octokit, owner, repo, draft.title, draft.body, labels);
      canonicalNumber = created.data ? created.data.number : null;
    } catch (error) {
      core.error(logMessage(this.config.logging.governance_create_canonical_failed, { error: error.message }));
      throw error; // handler 兜底：不关闭原 issue
    }

    if (!canonicalNumber) {
      throw new Error('创建 canonical 后未拿到编号，中止以便重试');
    }
    core.info(logMessage(this.config.logging.governance_canonical_created, { number: canonicalNumber }));

    const finalComment = this.render('governance_new_issue_comment', {
      summary,
      canonical: canonicalNumber
    });
    await this.safeComment(octokit, owner, repo, number, finalComment);
    await this.safeClose(octokit, owner, repo, number);

    core.info(logMessage(this.config.logging.governance_new_topic_log, { number }));
    return { decision: GOVERNANCE_DECISIONS.NEW_TOPIC, canonicalNumber, closed: true };
  }

  render(templateKey, params) {
    const template = this.config.responses[templateKey] || this.config.locale?.responses?.[templateKey] || '';
    let text = logMessage(template, params);
    text = `${text}\n\n${logMessage(this.config.responses.governance_log_prefix, { action: templateKey })}`;
    return text;
  }

  async postDryRun(octokit, owner, repo, issue, concreteComment, actionText) {
    const intro = this.config.responses.governance_dry_run;
    const body = `${intro}\n\n**本应执行**：${actionText}\n\n---\n\n${concreteComment}`;
    await this.safeComment(octokit, owner, repo, issue.number, body);
  }

  async safeComment(octokit, owner, repo, number, body) {
    try {
      await this.ops.addComment(octokit, owner, repo, number, body, this.config.logging.governance_comment_failed);
    } catch (error) {
      core.warning(logMessage(this.config.logging.governance_comment_failed, { error: error.message }));
    }
  }

  async safeAddLabels(octokit, owner, repo, number, labels) {
    try {
      await this.ops.addLabels(octokit, owner, repo, number, labels, this.config.logging.label_add_api_failed);
    } catch (error) {
      core.warning(logMessage(this.config.logging.label_add_failed, { error: error.message }));
    }
  }

  async safeClose(octokit, owner, repo, number) {
    try {
      await this.ops.updateIssueState(octokit, owner, repo, number, 'closed', 'not_planned');
    } catch (error) {
      // 关闭失败是已评论/已建 canonical 之后，可安全重试，日志留痕即可
      core.warning(logMessage(this.config.logging.issue_close_failed, { error: error.message }));
    }
  }

  async safeRecordMerge(octokit, owner, repo, canonicalNumber, issue, summary) {
    const record = this.render('governance_merge_canonical_record', {
      source: issue.number,
      author: issue.user?.login || 'unknown',
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
}

module.exports = IssueGovernanceService;
module.exports.DUPLICATE_PATTERN = DUPLICATE_PATTERN;
module.exports.splitTitleBody = splitTitleBody;