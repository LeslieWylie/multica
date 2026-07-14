// Same rationale as github-mark.tsx: lucide-react v1.x dropped brand marks,
// so inline the GitLab "tanuki" mark to keep MR rows/settings distinguishable
// from GitHub ones at a glance.
export function GitLabMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className={className} fill="currentColor">
      <path d="M23.6004 9.5927l-.0337-.0862L20.3.9814a.851.851 0 0 0-.3362-.405.8748.8748 0 0 0-.9997.0539.8748.8748 0 0 0-.29.4399l-2.2055 6.748H7.5375l-2.2057-6.748a.8573.8573 0 0 0-.29-.4412.8748.8748 0 0 0-.9997-.0539.8585.8585 0 0 0-.3362.405L.4332 9.5015l-.0325.0862a6.0657 6.0657 0 0 0 2.0119 7.0105l.0113.0087.0113.0075 7.1132 5.3325 3.6712 2.6944 2.2415 1.6417a.8823.8823 0 0 0 1.0475 0l2.2412-1.6417 3.6714-2.6944 7.1264-5.3417a6.0678 6.0678 0 0 0 2.0179-7.0105" />
    </svg>
  );
}
