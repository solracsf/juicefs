"""movefix.py SHA PICK_PREFIX NOTE : during the rebase, append `fixup SHA` (plus
the carry check) at the end of the group whose pick line starts with
PICK_PREFIX, in .git/rebase-merge/git-rebase-todo. Then reset the current
attempt (git reset --hard HEAD) and `git rebase --skip` it here. The full SHA
must be the h0 one (git rev-parse). Used for the two fixups that belong to
'hold snapshots against deletion' (created-time xattr, SQL hold prefix case)."""
import sys, subprocess
sha = subprocess.run(['git','rev-parse',sys.argv[1]],capture_output=True,text=True).stdout.strip()
after = sys.argv[2]
p='.git/rebase-merge/git-rebase-todo'; L=open(p).read().split('\n')
i=[k for k,l in enumerate(L) if l.startswith('pick '+after)][0]
j=i+1
while j < len(L) and not L[j].startswith('pick '): j+=1
L[j:j]=['fixup %s # (moved) %s'%(sha, sys.argv[3]),'exec python ../carry.py check']
open(p,'w').write('\n'.join(L)); print('inserted before line',j+1)
