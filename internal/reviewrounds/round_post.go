package reviewrounds

import "github.com/datapointchris/prs/internal/pullrequests"

// IsRoundPost says the review opens with a round's marker, of any version, from an author a
// round posts as. A pull request carrying one was reviewed by a round, whichever grammar the
// round wrote in, so this reads no version where Read reads only Version.
func IsRoundPost(review pullrequests.Review) bool {
	_, marked := markerOf(review.Body)
	return marked && outsiderWhy(review.Author, review.AuthorType, review.AuthorAssociation) == ""
}
