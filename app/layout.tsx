import type { Metadata } from "next";
import "./globals.css";

export const metadata:Metadata={title:"Relay Panel — 个人网络转发控制台",description:"基于 nftables、tc 与 Realm 的个人多节点端口转发面板。",icons:{icon:"/favicon.svg"},openGraph:{title:"Relay Panel",description:"个人网络转发控制台",images:[{url:"/og.png",width:1760,height:896}]},twitter:{card:"summary_large_image",title:"Relay Panel",description:"个人网络转发控制台",images:["/og.png"]}};
const themeScript=`(()=>{try{const mode=localStorage.getItem("relay-panel-theme")||"system";const theme=mode==="system"?(matchMedia("(prefers-color-scheme: dark)").matches?"dark":"light"):mode;document.documentElement.dataset.theme=theme;document.documentElement.dataset.themeMode=mode}catch{document.documentElement.dataset.theme="dark"}})()`;
export default function RootLayout({children}:{children:React.ReactNode}){return <html lang="zh-CN" suppressHydrationWarning><head><script dangerouslySetInnerHTML={{__html:themeScript}}/></head><body>{children}</body></html>}
