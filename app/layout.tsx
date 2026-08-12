import type { Metadata } from "next";
import "./globals.css";

export const metadata:Metadata={title:"Relay Panel — 个人网络转发控制台",description:"基于 nftables、tc 与 Realm 的个人多节点端口转发面板。",icons:{icon:"/favicon.svg"},openGraph:{title:"Relay Panel",description:"个人网络转发控制台",images:[{url:"/og.png",width:1760,height:896}]},twitter:{card:"summary_large_image",title:"Relay Panel",description:"个人网络转发控制台",images:["/og.png"]}};
export default function RootLayout({children}:{children:React.ReactNode}){return <html lang="zh-CN"><body>{children}</body></html>}
