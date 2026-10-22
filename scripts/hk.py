"""Hunk tool for manual conflict resolution during the rebase.
   hk.py list FILE [REV]          hunks of REV's diff for FILE, with match status in ours (:2:)
   hk.py carry FILE i,j note [REV] save hunks as carries (carry/NNN.json)
   hk.py apply FILE SPECS [REV]    rebuild FILE from ours (:2:, else HEAD) applying hunks;
                                   SPECS = i,j,... (full pre-image match, whitespace-insensitive)
                                   or i@ANCHOR[+OFF] (pure insertion after the unique line
                                   containing ANCHOR, shifted by OFF; OFF=-1 inserts before it)
   HK_U=1 env: diff context lines (default 3); U1 splits hunks whose context moved."""
import json, os, re, subprocess, sys, glob
HERE = os.path.dirname(os.path.abspath(__file__))
def git(*a, check=True): return subprocess.run(['git',*a],capture_output=True,text=True,encoding='utf-8',check=check).stdout
def norm(s): return re.sub(r'\s+','',s)
def hunks(rev, f):
    d = git('diff','-U'+os.environ.get('HK_U','3'),rev+'^',rev,'--',f); out=[]; cur=None
    for l in d.split('\n'):
        if l.startswith('@@'): cur=[]; out.append((l,cur))
        elif cur is not None and l and l[0] in ' -+': cur.append((l[0],l[1:]))
    return out
def find(lines,p):
    n=len(p); k=[norm(x) for x in p]
    return [i for i in range(len(lines)-n+1) if all(norm(lines[i+j])==k[j] for j in range(n))]
def ours(f):
    r = subprocess.run(['git','show',':2:'+f],capture_output=True,text=True,encoding='utf-8')
    if r.returncode: r = subprocess.run(['git','show','HEAD:'+f],capture_output=True,text=True,encoding='utf-8')
    return r.stdout.split('\n')
cmd,f = sys.argv[1],sys.argv[2]
rev = sys.argv[-1] if cmd!='list' and len(sys.argv)>(5 if cmd=='carry' else 4) else (sys.argv[3] if cmd=='list' and len(sys.argv)>3 else 'REBASE_HEAD')
rev = git('rev-parse',rev).strip()
hs = hunks(rev,f); lines = ours(f)
if cmd=='list':
    for i,(hdr,h) in enumerate(hs):
        pre=[x for t,x in h if t!='+']; m=find(lines,pre)
        print('#%d %s  %s  (+%d -%d)'%(i,hdr.split('@@')[1].strip(),'MATCH@%d'%(m[0]+1) if len(m)==1 else ('AMBIG' if m else 'NO'),
              sum(t=='+' for t,_ in h),sum(t=='-' for t,_ in h)))
elif cmd=='carry':
    idx=[int(x) for x in sys.argv[3].split(',')]; note=sys.argv[4]
    os.makedirs(os.path.join(HERE,'carry'),exist_ok=True)
    n=len(glob.glob(os.path.join(HERE,'carry','*.json*')))
    for k,i in enumerate(idx):
        json.dump({'file':f,'hunk':hs[i][1],'note':note,'rev':rev[:8]},open(os.path.join(HERE,'carry','%03d.json'%(n+k)),'w',encoding='utf-8'),indent=0)
        print('carried #%d as %03d'%(i,n+k))
elif cmd=='apply':
    specs=[x for x in sys.argv[3].split(',') if x] if sys.argv[3] else []
    anch={int(x.split('@',1)[0]):x.split('@',1)[1] for x in specs if '@' in x}
    idx=[int(x.split('@',1)[0]) for x in specs]
    for i in sorted(idx, reverse=True):
        h=hs[i][1]
        if i in anch:
            a,_,off=anch[i].partition('+'); off=int(off or 0)
            m=[k for k,l in enumerate(lines) if a in l]; assert len(m)==1,(i,a,m)
            add=[x for t,x in h if t=='+']; assert all(t!='-' for t,_ in h)
            at=m[0]+1+off; lines=lines[:at]+add+lines[at:]; continue
        pre=[x for t,x in h if t!='+']; m=find(lines,pre)
        assert len(m)==1,(i,m)
        s=m[0]; seg=lines[s:s+len(pre)]; post=[]; k=0
        for t,x in h:
            if t==' ': post.append(seg[k]); k+=1
            elif t=='-': k+=1
            else: post.append(x)
        lines=lines[:s]+post+lines[s+len(pre):]
    open(f,'w',encoding='utf-8',newline='').write('\n'.join(lines))
    if f.endswith('.go'): subprocess.run(['gofmt','-w',f])
    print('applied',idx)
