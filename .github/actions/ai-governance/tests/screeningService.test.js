const baseConfig = require('../config.json');
const { applyLocale } = require('../src/utils/config');
const ScreeningService = require('../src/services/screeningService');

function buildConfig() {
  const config = JSON.parse(JSON.stringify(baseConfig));
  applyLocale(config, 'zh-CN');
  return config;
}

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

const subject = {
  kind: 'pr',
  number: 42,
  title: 'feat: add caching layer',
  body: '希望网关加一层缓存，缓解后端压力。'
};

const index = [
  { number: 57, kind: 'issue', title: '支持缓存', labels: ['canonical'], state: 'closed', state_reason: 'not_planned', closed_at: null },
  { number: 30, kind: 'pr', title: 'feat: 缓存实现', labels: [], state: 'merged', state_reason: null, closed_at: null },
  { number: 12, kind: 'issue', title: '登录失败', labels: ['bug'], state: 'closed', state_reason: 'completed', closed_at: null },
  { number: 31, kind: 'pr', title: 'docs: readme', labels: [], state: 'open', state_reason: null, closed_at: null }
];

describe('ScreeningService.screen', () => {
  test('合法解析：候选返回且带上 relevance', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"candidates":[{"number":57,"kind":"issue","relevance":"direct"},{"number":30,"kind":"pr","relevance":"context"}]}\n```'
    ]);
    const svc = new ScreeningService(openai, 'model', config, {});

    const candidates = await svc.screen(subject, index);

    expect(candidates).toEqual([
      { number: 57, kind: 'issue', relevance: 'direct' },
      { number: 30, kind: 'pr', relevance: 'context' }
    ]);
  });

  test('确定性闸门：幻觉编号被剔除（number+kind 必须命中索引）', async () => {
    const config = buildConfig();
    const openai = makeOpenai([
      '```json\n{"candidates":[{"number":999,"kind":"issue","relevance":"direct"},{"number":57,"kind":"issue","relevance":"direct"},{"number":30,"kind":"issue","relevance":"context"}]}\n```'
    ]);
    const svc = new ScreeningService(openai, 'model', config, {});

    const candidates = await svc.screen(subject, index);

    // #999 不在索引；#30 是 pr 但候选说 issue → kind 不匹配，双双剔除
    expect(candidates).toEqual([{ number: 57, kind: 'issue', relevance: 'direct' }]);
  });

  test('上限截断：不超过 maxScreenedCandidates（默认 5）', async () => {
    const config = buildConfig();
    const bigIndex = [
      ...index,
      { number: 61, kind: 'issue', title: 'a', labels: [], state: 'closed', state_reason: null, closed_at: null },
      { number: 62, kind: 'issue', title: 'b', labels: [], state: 'closed', state_reason: null, closed_at: null },
      { number: 63, kind: 'issue', title: 'c', labels: [], state: 'closed', state_reason: null, closed_at: null },
      { number: 64, kind: 'issue', title: 'd', labels: [], state: 'closed', state_reason: null, closed_at: null }
    ];
    const openai = makeOpenai([
      '```json\n{"candidates":[' +
        [57, 30, 12, 31, 61, 62, 63, 64].map(n => `{"number":${n},"kind":"${n === 30 ? 'pr' : 'issue'}","relevance":"context"}`).join(',') +
        ']}\n```'
    ]);
    const svc = new ScreeningService(openai, 'model', config, {});

    const candidates = await svc.screen(subject, bigIndex);

    expect(candidates).toHaveLength(5);
  });

  test('不可解析输出：返回 []（调用方回落关键词启发式，绝不硬失败）', async () => {
    const config = buildConfig();
    const openai = makeOpenai(['完全不是 JSON 的一团文本']);
    const svc = new ScreeningService(openai, 'model', config, {});

    await expect(svc.screen(subject, index)).resolves.toEqual([]);
  });

  test('AI 抛错：返回 []（fail-soft，同样触发调用方回落）', async () => {
    const config = buildConfig();
    const openai = makeOpenai();
    openai._create.mockRejectedValue(new Error('screen boom'));
    const svc = new ScreeningService(openai, 'model', config, {});

    await expect(svc.screen(subject, index)).resolves.toEqual([]);
  });

  test('阶段一提示词只收紧凑索引 + 主题元信息（正文截断 1000，无评论无时间线）', async () => {
    const config = buildConfig();
    const longBody = 'x'.repeat(2500);
    const openai = makeOpenai(['```json\n{"candidates":[]}\n```']);
    const svc = new ScreeningService(openai, 'model', config, {});

    await svc.screen({ ...subject, body: longBody }, index);

    const input = JSON.parse(openai._create.mock.calls[0][0].messages[1].content);
    expect(input.subject.body.length).toBeLessThanOrEqual(1005);
    expect(input.subject.body).toMatch(/…\(截断\)$/);
    // 索引条目是紧凑形状：无 body/comments/timeline 字段
    expect(input.index[0]).toEqual({
      number: 57, kind: 'issue', title: '支持缓存', labels: ['canonical'],
      state: 'closed', state_reason: 'not_planned'
    });
    expect('body' in input.index[0]).toBe(false);
  });

  test('空候选：返回 []（调用方回落）', async () => {
    const config = buildConfig();
    const openai = makeOpenai(['```json\n{"candidates":[]}\n```']);
    const svc = new ScreeningService(openai, 'model', config, {});

    await expect(svc.screen(subject, index)).resolves.toEqual([]);
  });

  test('screening-model 配置存在时，阶段一使用该模型', async () => {
    const config = buildConfig();
    const openai = makeOpenai(['```json\n{"candidates":[]}\n```']);
    const svc = new ScreeningService(openai, 'model', config, { screeningModel: 'cheap-model' });

    await svc.screen(subject, index);

    expect(openai._create.mock.calls[0][0].model).toBe('cheap-model');
  });

  test('无 screening-model 配置：回落主模型', async () => {
    const config = buildConfig();
    const openai = makeOpenai(['```json\n{"candidates":[]}\n```']);
    const svc = new ScreeningService(openai, 'model', config, {});

    await svc.screen(subject, index);

    expect(openai._create.mock.calls[0][0].model).toBe('model');
  });
});
