const baseConfig = require('../config.json');
const { applyLocale, normalizeLanguage, parseInputs } = require('../src/utils/config');

function cloneConfig() {
  return JSON.parse(JSON.stringify(baseConfig));
}

describe('configuration', () => {
  test('uses chat completions by default', () => {
    expect(baseConfig.defaults.ai_api_type).toBe('chat-completions');
  });

  test('defaults unknown languages to English', () => {
    expect(normalizeLanguage('fr')).toBe('en');
    expect(applyLocale(cloneConfig(), 'fr')).toBe('en');
  });

  test('loads English responses by default', () => {
    const config = cloneConfig();
    applyLocale(config, 'en');

    expect(config.responses.issue_spam).toContain('This issue');
    expect(config.locale.answer_language).toBe('English');
  });

  test('loads Simplified Chinese when requested', () => {
    const config = cloneConfig();
    applyLocale(config, 'zh-CN');

    expect(config.responses.issue_spam).toContain('此Issue');
    expect(config.locale.answer_language).toBe('Simplified Chinese');
  });

  describe('parseInputs 数值防 NaN（C17/FIX-E）', () => {
    const core = require('@actions/core');

    beforeEach(() => {
      jest.resetModules();
      // 清空所有 INPUT_* 环境变量，避免宿主环境污染
      Object.keys(process.env).filter(k => k.startsWith('INPUT_')).forEach(k => delete process.env[k]);
    });

    afterEach(() => {
      Object.keys(process.env).filter(k => k.startsWith('INPUT_')).forEach(k => delete process.env[k]);
    });

    test('非数值输入回落默认值，而不是 NaN（NaN 会令 slice(0,NaN) 静默清空 related 列表）', () => {
      process.env.INPUT_MAX_RELATED_ISSUES = 'not-a-number';
      process.env.INPUT_RELATED_COMMENTS_PER_ISSUE = 'abc';
      process.env.INPUT_RELATED_BODY_TRUNCATE = '';
      process.env.INPUT_MAX_CANONICAL_INDEX = 'x50';
      process.env.INPUT_CANONICAL_BODY_TRUNCATE = '1500px';

      const result = parseInputs(cloneConfig());

      // Number.isFinite 守卫：解析失败回落 config.defaults
      expect(result.maxRelatedIssues).toBe(baseConfig.defaults.max_related_issues);
      expect(result.relatedCommentsPerIssue).toBe(baseConfig.defaults.related_comments_per_issue);
      expect(result.relatedBodyTruncate).toBe(baseConfig.defaults.related_body_truncate);
      expect(result.maxCanonicalIndex).toBe(baseConfig.defaults.max_canonical_index);
      expect(result.canonicalBodyTruncate).toBe(baseConfig.defaults.canonical_body_truncate);
    });

    test('正常数值输入照常解析', () => {
      process.env.INPUT_MAX_RELATED_ISSUES = '7';
      process.env.INPUT_MAX_CANONICAL_INDEX = '30';

      const result = parseInputs(cloneConfig());

      expect(result.maxRelatedIssues).toBe(7);
      expect(result.maxCanonicalIndex).toBe(30);
    });
  });

  describe('GOV_INPUTS 声明表（R15/C15.6）', () => {
    const GOV_INPUTS = require('../src/utils/config').GOV_INPUTS;

    test('每个声明项的 defaults 键都真实存在于 config.json（漏配即失败）', () => {
      const missing = GOV_INPUTS.filter(spec => !(spec.key in baseConfig.defaults))
        .map(spec => spec.key);
      expect(missing).toEqual([]);
    });

    test('action.yml 定义了表中的每个 input（漂移即失败）', () => {
      const fs = require('fs');
      const path = require('path');
      const yml = fs.readFileSync(path.join(__dirname, '..', 'action.yml'), 'utf8');
      const missing = GOV_INPUTS.filter(spec => !new RegExp(`^  ${spec.input}:`, 'm').test(yml))
        .map(spec => spec.input);
      expect(missing).toEqual([]);
    });

    test('env 名与 input 名的 snake 转换一致（INPUT_X 对应 x）', () => {
      for (const spec of GOV_INPUTS) {
        expect(spec.env).toBe('INPUT_' + spec.input.toUpperCase().replace(/-/g, '_'));
      }
    });
  });

  describe('parseInputs 两段式与历史语境参数（F1/F2 接线）', () => {
    beforeEach(() => {
      jest.resetModules();
      Object.keys(process.env).filter(k => k.startsWith('INPUT_')).forEach(k => delete process.env[k]);
    });

    afterEach(() => {
      Object.keys(process.env).filter(k => k.startsWith('INPUT_')).forEach(k => delete process.env[k]);
    });

    test('enable-two-stage / max-history-index / screening-model 缺省回落 config.defaults', () => {
      const result = parseInputs(cloneConfig());
      expect(result.enableTwoStage).toBe(false);
      expect(result.maxHistoryIndex).toBe(100);
      expect(result.maxScreenedCandidates).toBe(5);
      expect(result.screeningModel).toBe('');
    });

    test('显式输入覆盖 defaults（含 NaN 守卫）', () => {
      process.env.INPUT_ENABLE_TWO_STAGE = 'true';
      process.env.INPUT_MAX_HISTORY_INDEX = '60';
      process.env.INPUT_MAX_SCREENED_CANDIDATES = 'zzz'; // NaN → 回落默认 5
      process.env.INPUT_SCREENING_MODEL = 'cheap-model';
      const result = parseInputs(cloneConfig());
      expect(result.enableTwoStage).toBe(true);
      expect(result.maxHistoryIndex).toBe(60);
      expect(result.maxScreenedCandidates).toBe(5);
      expect(result.screeningModel).toBe('cheap-model');
    });
  });

  describe('loadConfig / validateConfig（R15 顺手补测）', () => {
    test('loadConfig 读取真实 config.json 并通过校验', () => {
      jest.resetModules();
      const { loadConfig } = require('../src/utils/config');
      const config = loadConfig();
      expect(config.prompts.history_screening).toBeTruthy();
      expect(config.defaults.enable_two_stage).toBe(false);
    });

    test('validateConfig 拒绝缺少必需段落的配置', () => {
      jest.resetModules();
      const { validateConfig } = require('../src/utils/config');
      expect(() => validateConfig({ prompts: {}, ai_settings: { max_tokens: 100, temperature: 0.1 } }))
        .toThrow('缺少必需的段落');
      expect(() => validateConfig({
        prompts: { spam_detection: 'x', readme_coverage_check: 'x', content_quality_check: 'x', pr_spam_detection: 'x' },
        responses: {}, logging: {},
        ai_settings: { max_tokens: 0, temperature: 0.1 }, defaults: {}
      })).toThrow('max_tokens');
    });
  });
});
