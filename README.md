# m-ui 流量奖励的公开账本

ledger.json 由登记服务生成、定时提交(.github/workflows/reward-ledger.yml),提交前先过一遍 deploy/reward/check-ledger.mjs。
每一笔入账都带着链上的交易号,可以拿到区块浏览器上核对;这个分支的提交历史就是奖池和积分每一次变动的记录。
