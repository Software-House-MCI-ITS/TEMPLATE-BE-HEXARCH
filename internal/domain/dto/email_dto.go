package dto

type NewUserData struct {
	UserEmail string
	Username  string
	Password  string
}

type VerifyEmailData struct {
	VerificationURL string
}

type ResetPasswordData struct {
	ResetLink string
}
