"use client";

import { Translation, useT } from "@/components/i18n-provider";
import { useEffect, useRef, useState } from "react";
import { RotateCw } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Status } from "./shared";
import { wsPath } from "@/lib/api";

export function WorkspaceTerminal({id}: {id:string}) {
  const { t } = useT();
  const node = useRef<HTMLDivElement>(null);
  const [attempt,setAttempt] = useState(0);
  const [status,setStatus] = useState("connecting");

  useEffect(() => {
    const target = node.current;
    if (!target) return;
    let disposed = false;
    let cleanup = () => {};
    Promise.all([import("@xterm/xterm"),import("@xterm/addon-fit")]).then(([{Terminal},{FitAddon}])=>{
      if(disposed) return;
      Terminal.strings.promptLabel = t("terminalInput");
      Terminal.strings.tooMuchOutput = t("terminalTooMuchOutput");
      const terminal = new Terminal({cursorBlink:true, fontSize:13, scrollback:1800,
        screenReaderMode: true, fontFamily:"\"JetBrains Mono Variable\", ui-monospace, SFMono-Regular, Menlo, monospace", theme:{background:"#141413",foreground:"#ecebe7",cursor:"#76a5f3",selectionBackground:"#2c3e63"}});
      const fit=new FitAddon();
      terminal.loadAddon(fit);
      terminal.open(target);
      terminal.attachCustomKeyEventHandler(e => {
        if(e.key==="Tab" && e.shiftKey) {
          if (e.type === "keydown") {
            e.preventDefault();
            document.getElementById("terminal-reconnect")?.focus();
          }
          return false;
        }
        return true;
      });
      const protocol = location.protocol==="https:" ? "wss:" : "ws:";
      const ws = new WebSocket(protocol+"//"+location.host+"/api/v1"+wsPath(id)+"/terminal");
      ws.binaryType="arraybuffer";
      const sendSize=()=>{
        fit.fit();
        if(ws.readyState===WebSocket.OPEN) ws.send(JSON.stringify({type:"resize",cols:terminal.cols,rows:terminal.rows}));
      };
      const ro = new ResizeObserver(sendSize);
      ro.observe(target);
      ws.onopen=()=>{setStatus("connected");sendSize();};
      ws.onmessage=(e: MessageEvent<ArrayBuffer>)=>{terminal.write(new Uint8Array(e.data));};
      ws.onclose=()=>{if(!disposed){setStatus("offline");terminal.writeln("\r\n" + t("shellDisconnected"));}};
      const encoder = new TextEncoder();
      // The API ends a terminal that receives a message over 64 KiB, so a large paste is sent in parts.
      const part = 32*1024;
      const send=(bytes: Uint8Array<ArrayBuffer>)=>{
        for(let i=0; i<bytes.length && ws.readyState===WebSocket.OPEN; i+=part) ws.send(bytes.subarray(i,i+part));
      };
      const input = terminal.onData(data=>send(encoder.encode(data)));
      const binary = terminal.onBinary(data=>send(Uint8Array.from(data,c=>c.charCodeAt(0))));
      cleanup=()=>{ro.disconnect();ws.onclose=null;ws.close();input.dispose();binary.dispose();terminal.dispose();};
    }).catch(()=>{if(!disposed)setStatus("offline");});
    return ()=>{disposed=true;cleanup();};
  },[id,attempt,t]);

  return <section className="terminal-frame" aria-labelledby="terminal-title">
    <div className="terminal-bar">
      <div className="min-w-0"><h2 id="terminal-title" className="text-[#ecebe7]">{t("developmentTerminal")}</h2>
        <p><Translation id="terminalHint" values={{ path: <code>/workspace/project</code> }}/></p></div>
      <div className="actions"><Status state={status} label={status==="offline"?t("disconnected"):undefined}/>
        <Button id="terminal-reconnect" variant="outline" size="sm" className="border-[#3b3b37] bg-transparent text-[#ecebe7] hover:bg-[#242422] hover:border-[#75746d]"
          onClick={()=>{setStatus("connecting");setAttempt(n=>n+1);}}><RotateCw data-icon="inline-start"/>{t("reconnect")}</Button></div>
    </div>
    <div className="terminal-surface"><div ref={node}/></div>
  </section>;
}
