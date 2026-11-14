功德箱的数据,由 .github/workflows/donors.yml 自动更新,不要手改 donors.json。
要屏蔽一个名字:把它加进 state.json 的 blocked。

## 手动记账(场外捐赠)

没有经过面板留名、对不上链上到账的捐赠(比如直接转账后私下告知的),在 state.json 的 donors 里按名字记:

- 值是微 USDT 整数(1 USDT = 1000000),记的是这个名字的累计总额,不是单笔;
- 这个名字以后再有留名并对上的链上到账,会自动加在这个数上;
- 改完 state.json 后按同样的规则重新生成 donors.json(按金额从大到小,金额是 USDT、保留三位小数),或者等下一次定时运行重新生成。
