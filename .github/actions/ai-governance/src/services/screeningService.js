const core = require('@actions/core');
const { logMessage } = require('../utils/helpers');
const { callAIStructured } = require('./ai');
const { GOVERNANCE_DEFAULTS } = require('../utils/constants');

const SUBJECT_BODY_TRUNCATE = 1000;

/**
 * 两段式流水线的阶段一：廉价筛选 AI（spec F2）。
 *
 * 输入只有「主题元信息 + 紧凑索引」—— 永远不喂正文全文、评论、时间线
 * （阶段二的深度材料由脚本经 HistoryContextService.enrich 补全，AI 无网络原则不变）。
 *
 * 确定性闸门（screeningGate，与 verifyEvidence 同族）：
 *   - 每个候选必须真实存在于提供的索引（number+kind 双匹配）—— 幻觉编号剔除；
 *   - 上限 gov.maxScreenedCandidates（默认 5）；
 *   - 输出不可解析 / AI 抛错 → []（调用方回落关键词启发式，绝不硬失败）。
 */
class ScreeningService {
  /**
   * @param {Object} openai OpenAI 客户端
   * @param {string} aiModel 主模型名
   * @param {Object} config 合并后的配置
   * @param {Object} gov 治理参数（screeningModel / maxScreenedCandidates）
   */
  constructor(openai, aiModel, config, gov = {}) {
    this.openai = openai;
    this.aiModel = aiModel;
    this.config = config;
    this.gov = { ...GOVERNANCE_DEFAULTS, ...gov };
  }

  /**
   * @param {Object} subject { kind: 'issue'|'pr', number, title, body }
   * @param {Array} index HistoryContextService.buildIndex 的紧凑索引
   * @returns {Promise<Array<{number, kind, relevance}>>}
   */
  async screen(subject, index) {
    if (!Array.isArray(index) || index.length === 0) {
      return [];
    }

    const truncate = (text, limit) => {
      const s = String(text || '');
      return s.length > limit ? `${s.slice(0, limit)}…(截断)` : s;
    };

    const compactIndex = index.map(item => ({
      number: item.number,
      kind: item.kind,
      title: item.title,
      labels: item.labels || [],
      state: item.state,
      state_reason: item.state_reason
    }));

    const request = {
      instructions: this.config.prompts.history_screening,
      input: JSON.stringify({
        subject: {
          kind: subject.kind,
          number: subject.number,
          title: subject.title,
          body: truncate(subject.body, SUBJECT_BODY_TRUNCATE)
        },
        index: compactIndex
      })
    };

    let result;
    try {
      result = await callAIStructured(
        this.openai,
        this.gov.screeningModel || this.aiModel,
        request,
        this.config,
        '历史语境筛选',
        { candidates: [] }
      );
    } catch (error) {
      // fail-soft：筛选失败回落关键词启发式（C16），绝不硬失败
      core.warning(logMessage(this.config.logging.history_screening_failed, { error: error.message }));
      return [];
    }

    if (!result || !Array.isArray(result.candidates)) {
      return [];
    }

    return this.screeningGate(result.candidates, index);
  }

  /**
   * 确定性闸门：幻觉剔除 + 上限截断。
   */
  screeningGate(candidates, index) {
    const realKeys = new Set(index.map(i => `${i.kind}:${i.number}`));
    const seen = new Set();
    const valid = [];
    for (const c of candidates) {
      const number = parseInt(c.number, 10);
      const kind = c.kind === 'pr' ? 'pr' : 'issue';
      if (!Number.isFinite(number) || !realKeys.has(`${kind}:${number}`)) {
        continue; // 幻觉编号 / kind 不匹配 → 剔除
      }
      const key = `${kind}:${number}`;
      if (seen.has(key)) {
        continue;
      }
      seen.add(key);
      valid.push({
        number,
        kind,
        relevance: ['direct', 'context', 'tangential'].includes(c.relevance) ? c.relevance : 'context'
      });
      if (valid.length >= this.gov.maxScreenedCandidates) {
        break;
      }
    }
    return valid;
  }
}

module.exports = ScreeningService;
