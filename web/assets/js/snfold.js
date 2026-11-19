// 节点设置页进来时要不要按线路收起:节点不多(一屏放得下)就全展开 —— 只有两三条线路的面板不用再一条条点开;
// 多了才按线路收起(几十条线路、上百个节点全摊开找不到东西)。单拿出来是为了能单测(页面模块本身要 DOM)。
export const SMALL_SETUP = 12; // 节点数不超过这么多算「一屏放得下」

// counts 是每条线路的节点数
export const expandByDefault = counts => counts.reduce((a, n) => a + n, 0) <= SMALL_SETUP;
