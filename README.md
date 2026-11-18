# 伪装站图包 / Camouflage photo pack

m-ui「页面伪装」里视频站、个人博客、软件下载页的封面照片:82 张 1280×720 JPEG,AI 生成,画面里没有文字、商标和看得清的人脸。

服务器只在用到时取这一站用到的那几张,按程序里 `camo/media.go` 的 sha256 清单核对,再按本机的随机种子裁切、调色、重新压缩后才放上伪装站,所以每台服务器上的图都不一样,也和这里的原图对不上字节。网页里不会引用这里的地址。

文件名:`类别-天色-两位序号.jpg`。换图、加图要同时改 `camo/media.go` 的清单;旧版本程序只认它自己清单里的文件,这里的旧文件不要删。

---

Cover photos for the video site, personal blog and software download page in m-ui's page camouflage: 82 AI-generated 1280×720 JPEGs with no text, logos or recognizable faces. A server fetches only the few its site uses, checks them against the sha256 list in `camo/media.go`, then crops, tones and re-encodes them with its own random seed, so no two servers serve the same bytes and none matches these originals. Pages never link here. Keep old files: older releases only know their own list.
