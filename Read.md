1. 用到的技术栈 gortsplib(一个开源的RTSP客户端服务端)
2. WebCodecs, 前端解码NALU
3. 如果为H264目前最新的Chrom和Edge浏览器可以正常支持
4. 如果为H265需要Windows下正常安装插件
    a. 打开Win+R 输入ms-windows-store://pdp/?ProductId=9n4wgh0z6vhq 安装插件
    b. 打开powershell 输入 Get-AppxPackage Microsoft.HEVCVideoExtension 验证插件是否安装
5. Safari 浏览器据说支持H265解码不需要安装插件，没有设备无法验证
