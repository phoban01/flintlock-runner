# Grow the thin pool by a fifth whenever it is four fifths full, out of the
# headroom flr-thin-pool.service leaves in the volume group.
activation {
	thin_pool_autoextend_threshold=80
	thin_pool_autoextend_percent=20
}
